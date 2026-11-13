package bandstate

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Config is the mirror's critic configuration (model.BandRef on the core side).
type Config struct {
	Socket  string   `json:"socket"`
	Utility []string `json:"utility,omitempty"`
	Repo    string   `json:"repo,omitempty"`
	// LoadEvery is the heartbeat cadence; 0 is UpdatedEvery.
	LoadEvery time.Duration `json:"-"`
}

// DefaultRepo is where the hub's branch-carrying jobs (aether.*) build from.
const DefaultRepo = "bic/aether"

// TipEvery bounds forge calls for the superseded check.
const TipEvery = 5 * time.Minute

// State is what the mirror carries from one round to the next (the plugin's
// opaque state): the view it last wrote, and the cached branch tips.
type State struct {
	View *View `json:"view,omitempty"`
	Slim bool  `json:"slim,omitempty"` // the Dropped keys have been unset
	// Rows: queues and nodes are on the form as objects keyed by queue/node
	// (one row a leaf), not as arrays, which a diff can only replace whole.
	Rows bool `json:"rows,omitempty"`
	// UpdatedAt is when the updated key was last written (the hub's clock).
	UpdatedAt int64             `json:"updated_at,omitempty"`
	Tips      map[string]string `json:"tips,omitempty"` // branch -> tip sha ("" = absent upstream: unknown)
	TipsAt    time.Time         `json:"tips_at,omitempty"`
	TipErr    string            `json:"tip_err,omitempty"`
	// Desc caches descent verdicts, Pair(job sha, tip) -> tip strictly
	// descends from sha: one forge call per new pair. An error is not a
	// verdict: it is retried, no sooner than TipEvery after it.
	Desc      map[string]bool `json:"desc,omitempty"`
	DescErr   string          `json:"desc_err,omitempty"`
	DescErrAt time.Time       `json:"desc_err_at,omitempty"`
}

// DescFunc says whether head is a strict descendant of base in repo (the
// forge's compare: ahead, behind by 0). A definite no (behind, diverged,
// identical, unknown sha, no compare) is false with no error. Nil skips the
// check: nothing is ever flagged superseded.
type DescFunc func(ctx context.Context, repo, base, head string) (bool, error)

// TipFunc asks the forge for a branch's tip in repo ("" when it is gone).
// Nil skips the superseded check.
type TipFunc func(ctx context.Context, repo, branch string) (string, error)

// Result is one round: the next state, and the form keys to write. Set is
// empty when nothing changed: a quiet hub writes nothing, so studiers see no
// diff. Updated (the hub's now) is written only alongside a change.
type Result struct {
	State   State
	Set     map[string]json.RawMessage
	Unset   []string
	Changed []string
}

// Step turns one hub read and the last state into the next state and the
// keys to write. It is pure apart from tip, which it calls at most every
// TipEvery (now is the caller's clock, for that cadence only).
func Step(ctx context.Context, prev State, h *Hub, cfg Config, now time.Time, tip TipFunc, desc DescFunc) Result {
	next := prev
	refreshTips(ctx, &next, cfg, Branches(h.Live), now, tip)
	refreshDesc(ctx, &next, cfg, pairs(h.Live, next.Tips), now, desc)
	view := Render(h, cfg.Utility, next.Tips, next.Desc)
	next.View = &view

	r := Result{State: next, Set: map[string]json.RawMessage{}}
	var was View
	if prev.View != nil {
		was = *prev.View
	}
	put := func(k string, v any) {
		b, _ := json.Marshal(v)
		r.Set[k] = b
		r.Changed = append(r.Changed, k)
	}
	fresh := !prev.Rows // first round of this shape: every key once
	// On change: what a node IS (class, disk tier, drained, stale) and the dupes.
	if nodes := NodeRows(view.Nodes); fresh || !sameMap(NodeRows(was.Nodes), nodes) {
		put("nodes", nodes)
	}
	if fresh || !same(was.Dupes, view.Dupes) {
		put("dupes", view.Dupes)
	}
	// At most every UpdatedEvery: what moves with every job (queue depths, the
	// oldest ready job, free slots, running jobs, free disk, draining), and
	// updated. A quiet hub still gets updated, so a reader sees it is watched.
	every := cfg.LoadEvery
	if every <= 0 {
		every = UpdatedEvery
	}
	if fresh || h.Stats.Hub.Now-prev.UpdatedAt >= every.Milliseconds() {
		put("queues", Rows(view.Queues))
		put("node_load", LoadRows(view.Nodes))
		b, _ := json.Marshal(time.UnixMilli(h.Stats.Hub.Now).UTC().Format(time.RFC3339))
		r.Set["updated"] = b
		r.State.UpdatedAt = h.Stats.Hub.Now
	}
	r.State.Rows = true
	if !prev.Slim {
		r.Unset, r.State.Slim = append([]string(nil), Dropped...), true
	}
	return r
}

// UpdatedEvery bounds the heartbeat keys (queues, node_load, updated).
const UpdatedEvery = 10 * time.Minute

// volatile are the node fields that move with every job: they go to
// node_load at the heartbeat cadence, never to the nodes row.
var volatile = map[string]bool{"free_slots": true, "running": true, "disk_free": true, "draining": true}

// NodeRows is the nodes key: per node, its name and what it IS.
func NodeRows(lines []string) map[string]string { return splitRows(lines, false) }

// LoadRows is the node_load key: per node, its name and what moves.
func LoadRows(lines []string) map[string]string { return splitRows(lines, true) }

func splitRows(lines []string, load bool) map[string]string {
	out := make(map[string]string, len(lines))
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		keep := []string{f[0]}
		for _, t := range f[1:] {
			k, _, _ := strings.Cut(t, "=")
			if volatile[k] == load {
				keep = append(keep, t)
			}
		}
		out[RowKey(f[0])] = strings.Join(keep, " ")
	}
	return out
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Rows keys each line by its first field (the queue or node name), so a change
// to one queue or node patches one leaf. The value is the whole line, as the
// array carried it. A dot in the name becomes "_": a dotted member key is a
// path to the form (figaro#43), and queue names carry dots (gate.change).
func Rows(lines []string) map[string]string {
	out := make(map[string]string, len(lines))
	for _, l := range lines {
		name, _, _ := strings.Cut(l, " ")
		out[RowKey(name)] = l
	}
	return out
}

// RowKey is a queue or node name as a member key: dots become "_".
func RowKey(name string) string { return strings.ReplaceAll(name, ".", "_") }

// refreshTips asks for each live branch's tip at most every TipEvery. A
// failure keeps the old tips and records the error; no live branch empties
// the cache.
func refreshTips(ctx context.Context, st *State, cfg Config, branches []string, now time.Time, tip TipFunc) {
	if len(branches) == 0 {
		st.Tips, st.TipErr = nil, ""
		return
	}
	if tip == nil || now.Sub(st.TipsAt) < TipEvery {
		return
	}
	repo := cfg.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	tips := map[string]string{}
	for _, b := range branches {
		t, err := tip(ctx, repo, b)
		if err != nil {
			st.TipErr = err.Error()
			return
		}
		tips[b] = t
	}
	st.Tips, st.TipsAt, st.TipErr = tips, now.UTC(), ""
}

// refreshDesc asks the forge for each pair without a verdict, keeps only the
// pairs live jobs still need, and backs off TipEvery after an error.
func refreshDesc(ctx context.Context, st *State, cfg Config, need map[string][2]string, now time.Time, desc DescFunc) {
	kept := map[string]bool{}
	for k := range need {
		if v, ok := st.Desc[k]; ok {
			kept[k] = v
		}
	}
	st.Desc = kept
	if len(st.Desc) == 0 {
		st.Desc = nil
	}
	if desc == nil || (st.DescErr != "" && now.Sub(st.DescErrAt) < TipEvery) {
		return
	}
	repo := cfg.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := st.Desc[k]; ok {
			continue
		}
		p := need[k]
		ok, err := desc(ctx, repo, p[0], p[1])
		if err != nil {
			st.DescErr, st.DescErrAt = err.Error(), now.UTC()
			return
		}
		if st.Desc == nil {
			st.Desc = map[string]bool{}
		}
		st.Desc[k] = ok
	}
	st.DescErr, st.DescErrAt = "", time.Time{}
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
