package bandstate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// View is the mirror's form keys, each a list of lines. Updated is set by the
// round, only when another key changed. It is SLIM on purpose (warden
// 2026-11-09): a study mirrors the whole form on every change, so there is no
// per-job list; `band-hub list` is the on-demand detail.
type View struct {
	Queues []string `json:"queues"`
	Nodes  []string `json:"nodes"`
	Dupes  []string `json:"dupes"`
}

// Keys are the form keys the mirror owns, in View order.
var Keys = []string{"queues", "nodes", "dupes"}

// Dropped are keys earlier versions wrote; a round unsets them once.
var Dropped = []string{"live"}

func (v View) Map() map[string][]string {
	return map[string][]string{"queues": v.Queues, "nodes": v.Nodes, "dupes": v.Dupes}
}

// Since renders an instant as a time, never as an age: an age changes on
// every poll and would rewrite the studied form each time, while the instant
// a job started never changes. HH:MMZ on the hub's own day, else MM-DDTHH:MMZ.
func Since(ms, nowMS int64) string {
	t, now := time.UnixMilli(ms).UTC(), time.UnixMilli(nowMS).UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04Z")
	}
	return t.Format("01-02T15:04Z")
}

// diskGiB floors free space to 10 GiB steps: builds write constantly.
func diskGiB(b int64) string {
	g := b >> 30
	return fmt.Sprintf("%dG", g/10*10)
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (j Job) param(k string) string {
	if raw, ok := j.Payload.Params[k]; ok {
		return str(raw)
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

func (j Job) what() string {
	w := j.param("branch")
	if w == "" {
		w = j.param("label")
	}
	if w == "" {
		w = "-"
	}
	w = strings.Join(strings.Fields(w), "_")
	if len(w) > 48 {
		w = w[:47] + "~"
	}
	return w
}

// Render builds the view. tips maps branch -> tip sha; desc maps Pair(job sha,
// tip) -> whether the tip is a proven strict descendant of the job's sha. Only
// a proven descendant is flagged; anything else (absent upstream, no verdict,
// behind, diverged) is UNKNOWN: the forge lags the node mirrors, and
// mirror-only branches never reach it.
func Render(h *Hub, utility []string, tips map[string]string, desc map[string]bool) View {
	now := h.Stats.Hub.Now
	v := View{Queues: []string{}, Nodes: []string{}, Dupes: []string{}}

	qs := append([]QueueStats(nil), h.Stats.Queues...)
	sort.Slice(qs, func(i, j int) bool { return qs[i].Queue < qs[j].Queue })
	for _, q := range qs {
		if q.Counts.Ready+q.Counts.Leased == 0 {
			continue
		}
		// The oldest ready job, from the jobs themselves: its id and queue INSTANT (an age, or the hub's
		// wait percentiles, would move on every poll and rewrite the studied form).
		oldest := "-"
		var first Job
		for _, j := range h.Live {
			if j.Queue == q.Queue && j.State == "ready" && (first.ID == 0 || j.Created < first.Created || j.Created == first.Created && j.ID < first.ID) {
				first = j
			}
		}
		if first.ID != 0 {
			oldest = fmt.Sprintf("%d@%s", first.ID, Since(first.Created, now))
		}
		v.Queues = append(v.Queues, fmt.Sprintf("%s ready=%d leased=%d oldest_ready=%s", q.Queue, q.Counts.Ready, q.Counts.Leased, oldest))
	}

	util := map[string]bool{}
	for _, u := range utility {
		util[u] = true
	}
	ns := append([]Node(nil), h.Nodes...)
	sort.Slice(ns, func(i, j int) bool { return ns[i].Node < ns[j].Node })
	for _, n := range ns {
		class := "heavy"
		if util[n.Node] {
			class = "utility"
		}
		s := n.Snapshot
		var running []string
		for _, r := range s.Running {
			running = append(running, fmt.Sprint(r.HubJob))
		}
		sort.Strings(running)
		run := "-"
		if len(running) > 0 {
			run = strings.Join(running, ",")
		}
		line := fmt.Sprintf("%s class=%s free_slots=%d running=%s disk_free=%s", n.Node, class,
			max(s.Workers.Total-s.Workers.Busy, 0), run, diskGiB(s.Disk.FreeBytes))
		if g := s.Disk.Guard; g != nil {
			line += fmt.Sprintf(" tier=%d", g.Tier)
			if g.Drained {
				line += " drained"
			}
		}
		if s.Draining != nil && s.Draining != false {
			line += " draining"
		}
		if stale := h.Stats.Hub.NodeStaleS; stale > 0 && n.AgeS > stale && n.Received > 0 {
			line += " stale_since=" + Since(n.Received, now)
		}
		v.Nodes = append(v.Nodes, line)
	}

	live := append([]Job(nil), h.Live...)
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.Queue != b.Queue {
			return a.Queue < b.Queue
		}
		return a.ID < b.ID
	})
	v.Dupes = append(append([]string{}, dupes(live)...), superseded(live, tips, desc)...)
	return v
}

// dupes groups live jobs that do the same work: same queue, template and the
// FULL canonical params (sorted JSON of payload.params, attrs and tests
// included), as warden-submit dedupes. The metadata view ebac reads shows only
// branch, sha, label and node, so the full params arrive as the hub's digest
// params_sum; a job without one is never grouped (flake.check 4709/4710: same
// sha, attrs ci vs keel-web-*, were flagged and a real job was cancelled).
func dupes(live []Job) []string {
	groups := map[string][]int64{}
	var order []string
	for _, j := range live {
		if j.Payload.ParamsSum == "" {
			continue
		}
		key := j.Queue + "|" + j.Payload.Template + "|" + j.Payload.ParamsSum
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], j.ID)
	}
	var out []string
	for _, key := range order {
		ids := groups[key]
		if len(ids) < 2 {
			continue
		}
		var first Job
		for _, j := range live {
			if j.ID == ids[0] {
				first = j
			}
		}
		s := make([]string, len(ids))
		for i, id := range ids {
			s[i] = fmt.Sprint(id)
		}
		out = append(out, fmt.Sprintf("dupe %s %s: %s", first.Queue, short(first.param("sha")), strings.Join(s, ",")))
	}
	sort.Strings(out)
	return out
}

// trunk branches move all the time and a job pinned to one of their shas
// (a nightly eval) is not superseded by the move: never flagged.
var trunk = map[string]bool{"main": true, "master": true}

// superseded names live jobs whose branch tip upstream is a PROVEN strict
// descendant of their sha (warden 2026-11-10: jobs 4664-4669 were flagged by
// an older tip). No tip, no verdict, or a negative one is never flagged.
func superseded(live []Job, tips map[string]string, desc map[string]bool) []string {
	var out []string
	for _, j := range live {
		br, sha := j.param("branch"), j.param("sha")
		if br == "" || sha == "" || trunk[br] {
			continue
		}
		tip := tips[br]
		if tip == "" || tip == sha || !desc[Pair(sha, tip)] {
			continue
		}
		out = append(out, fmt.Sprintf("superseded %d %s %s %s tip=%s", j.ID, j.Queue, j.what(), short(sha), short(tip)))
	}
	return out
}

// Pair keys a descent verdict: the job's sha, then the tip.
func Pair(sha, tip string) string { return sha + "..." + tip }

// pairs are the (job sha, tip) pairs live jobs need a descent verdict for.
func pairs(live []Job, tips map[string]string) map[string][2]string {
	out := map[string][2]string{}
	for _, j := range live {
		br, sha := j.param("branch"), j.param("sha")
		if br == "" || sha == "" || trunk[br] {
			continue
		}
		if tip := tips[br]; tip != "" && tip != sha {
			out[Pair(sha, tip)] = [2]string{sha, tip}
		}
	}
	return out
}

// Branches are the distinct branches live jobs build, for the tip check.
func Branches(live []Job) []string {
	seen := map[string]bool{}
	var out []string
	for _, j := range live {
		if b := j.param("branch"); b != "" && j.param("sha") != "" && !trunk[b] && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}
