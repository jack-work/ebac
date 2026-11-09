package bandstate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LiveCap bounds key live: studied values are mirrored whole.
const LiveCap = 60

// View is the mirror's form keys, each a list of lines. Updated is set by the
// round, only when another key changed.
type View struct {
	Queues []string `json:"queues"`
	Nodes  []string `json:"nodes"`
	Live   []string `json:"live"`
	Dupes  []string `json:"dupes"`
}

// Keys are the form keys the mirror owns, in View order.
var Keys = []string{"queues", "nodes", "live", "dupes"}

func (v View) Map() map[string][]string {
	return map[string][]string{"queues": v.Queues, "nodes": v.Nodes, "live": v.Live, "dupes": v.Dupes}
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

func (j Job) prio() int {
	if j.Priority == nil {
		return 0
	}
	return *j.Priority
}

// node is the build node a lease owner names: uid:1000:ross/w1 -> ross/w1.
func (j Job) node() string {
	if j.LeaseOwner == nil || *j.LeaseOwner == "" {
		return "-"
	}
	o := *j.LeaseOwner
	if i := strings.LastIndex(o, ":"); i >= 0 {
		o = o[i+1:]
	}
	return o
}

// since is when a ready job was queued, or a leased one claimed.
func (j Job) since(nowMS int64) string {
	from := j.Created
	if j.State == "leased" && j.Claimed != nil {
		from = *j.Claimed
	}
	return Since(from, nowMS)
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

// Render builds the view. tips maps branch -> tip sha ("" = gone); a branch
// absent from tips is not checked (no claim either way).
func Render(h *Hub, utility []string, tips map[string]string) View {
	now := h.Stats.Hub.Now
	v := View{Queues: []string{}, Nodes: []string{}, Live: []string{}, Dupes: []string{}}

	qs := append([]QueueStats(nil), h.Stats.Queues...)
	sort.Slice(qs, func(i, j int) bool { return qs[i].Queue < qs[j].Queue })
	for _, q := range qs {
		if q.Counts.Ready+q.Counts.Leased == 0 {
			continue
		}
		// The oldest ready job's queue time, from the jobs themselves: exact, so it does not jitter between polls.
		oldest, wait := "-", "-"
		var first int64
		for _, j := range h.Live {
			if j.Queue == q.Queue && j.State == "ready" && (first == 0 || j.Created < first) {
				first = j.Created
			}
		}
		if first != 0 {
			oldest = Since(first, now)
		}
		if q.WaitS.P50 != nil {
			wait = fmt.Sprintf("%dm", int(*q.WaitS.P50/60))
		}
		v.Queues = append(v.Queues, fmt.Sprintf("%s ready=%d leased=%d oldest_ready_since=%s wait_p50=%s",
			q.Queue, q.Counts.Ready, q.Counts.Leased, oldest, wait))
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

	// Which node runs a job, from the nodes' own reports: the meta view may not carry a lease owner.
	onNode := map[int64]string{}
	for _, n := range h.Nodes {
		for _, r := range n.Snapshot.Running {
			onNode[r.HubJob] = n.Node
		}
	}
	live := append([]Job(nil), h.Live...)
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.Queue != b.Queue {
			return a.Queue < b.Queue
		}
		if pa, pb := a.prio(), b.prio(); pa != pb {
			return pa > pb
		}
		return a.ID < b.ID
	})
	for i, j := range live {
		if i == LiveCap {
			v.Live = append(v.Live, fmt.Sprintf("+%d more", len(live)-LiveCap))
			break
		}
		prio := "-"
		if j.Priority != nil {
			prio = fmt.Sprint(*j.Priority)
		}
		node := j.node()
		if node == "-" && onNode[j.ID] != "" {
			node = onNode[j.ID]
		}
		v.Live = append(v.Live, fmt.Sprintf("%d %s %s %s %s %s %s", j.ID, j.Queue, prio, node, j.since(now), j.what(), short(j.param("sha"))))
	}

	v.Dupes = append(append([]string{}, dupes(live)...), superseded(live, tips)...)
	return v
}

// ignoredParams never distinguish two jobs: branch (the order), and the
// cosmetic label and why a resubmit may change (ebac Maintainer 2026-11-09).
var ignoredParams = map[string]bool{"branch": true, "label": true, "why": true}

// dupes groups live jobs that do the same work: same queue, sha and params
// with branch, label and why ignored.
func dupes(live []Job) []string {
	groups := map[string][]int64{}
	var order []string
	for _, j := range live {
		ks := make([]string, 0, len(j.Payload.Params))
		for k := range j.Payload.Params {
			if !ignoredParams[k] {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		var b strings.Builder
		fmt.Fprintf(&b, "%s|%s", j.Queue, j.Payload.Template)
		for _, k := range ks {
			fmt.Fprintf(&b, "|%s=%s", k, j.Payload.Params[k])
		}
		key := b.String()
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

// superseded names live jobs whose branch tip has moved off their sha (or
// whose branch is gone).
func superseded(live []Job, tips map[string]string) []string {
	var out []string
	for _, j := range live {
		br, sha := j.param("branch"), j.param("sha")
		if br == "" || sha == "" || trunk[br] {
			continue
		}
		tip, known := tips[br]
		if !known || tip == sha {
			continue
		}
		t := "gone"
		if tip != "" {
			t = short(tip)
		}
		out = append(out, fmt.Sprintf("superseded %d %s %s %s tip=%s", j.ID, j.Queue, j.what(), short(sha), t))
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
