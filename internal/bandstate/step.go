package bandstate

import (
	"context"
	"encoding/json"
	"time"
)

// Config is the mirror's critic configuration (model.BandRef on the core side).
type Config struct {
	Socket  string   `json:"socket"`
	Utility []string `json:"utility,omitempty"`
	Repo    string   `json:"repo,omitempty"`
}

// DefaultRepo is where the hub's branch-carrying jobs (aether.*) build from.
const DefaultRepo = "bic/aether"

// TipEvery bounds forge calls for the superseded check.
const TipEvery = 5 * time.Minute

// State is what the mirror carries from one round to the next (the plugin's
// opaque state): the view it last wrote, and the cached branch tips.
type State struct {
	View   *View             `json:"view,omitempty"`
	Tips   map[string]string `json:"tips,omitempty"` // branch -> tip sha ("" = branch gone)
	TipsAt time.Time         `json:"tips_at,omitempty"`
	TipErr string            `json:"tip_err,omitempty"`
}

// TipFunc asks the forge for a branch's tip in repo ("" when it is gone).
// Nil skips the superseded check.
type TipFunc func(ctx context.Context, repo, branch string) (string, error)

// Result is one round: the next state, and the form keys to write. Set is
// empty when nothing changed: a quiet hub writes nothing, so studiers see no
// diff. Updated (the hub's now) is written only alongside a change.
type Result struct {
	State   State
	Set     map[string]json.RawMessage
	Changed []string
}

// Step turns one hub read and the last state into the next state and the
// keys to write. It is pure apart from tip, which it calls at most every
// TipEvery (now is the caller's clock, for that cadence only).
func Step(ctx context.Context, prev State, h *Hub, cfg Config, now time.Time, tip TipFunc) Result {
	next := prev
	refreshTips(ctx, &next, cfg, Branches(h.Live), now, tip)
	view := Render(h, cfg.Utility, next.Tips)
	next.View = &view

	r := Result{State: next, Set: map[string]json.RawMessage{}}
	cur, old := view.Map(), map[string][]string{}
	if prev.View != nil {
		old = prev.View.Map()
	}
	for _, k := range Keys {
		if prev.View != nil && same(old[k], cur[k]) {
			continue
		}
		b, _ := json.Marshal(cur[k])
		r.Set[k] = b
		r.Changed = append(r.Changed, k)
	}
	if len(r.Set) > 0 {
		b, _ := json.Marshal(time.UnixMilli(h.Stats.Hub.Now).UTC().Format(time.RFC3339))
		r.Set["updated"] = b
	}
	return r
}

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
