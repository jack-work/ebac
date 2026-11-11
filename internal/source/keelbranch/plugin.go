package keelbranch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/keel"
)

// Plugin serves keel-branches. Keel and Now replace keel and the clock in
// tests; nil means the real ones.
type Plugin struct {
	Keel Keel
	Now  func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: keel branches have no seat of their own and no brief", req.Op)
	}
	w := &req.Critic
	if w.Branches == nil || !strings.Contains(w.Branches.Repo, "/") {
		return nil, fmt.Errorf("%s: keel branches without an owner/repo", w.Name)
	}
	k := pl.Keel
	if k == nil {
		k = keel.ForRef(&model.KeelRef{Repo: w.Branches.Repo, URL: w.Branches.URL, TokenCmd: w.Branches.TokenCmd})
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	var prev State
	if len(w.Runtime.KeelBranches) > 0 {
		if err := json.Unmarshal(w.Runtime.KeelBranches, &prev); err != nil {
			prev = State{}
		}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	r, err := Run(ctx, k, prev, Pass{Watch: *w.Branches, Children: req.Children, Owned: req.Owned, Max: model.MaxChildren, Now: now()})
	for _, l := range r.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if err != nil {
		// A round that could not read keel is a failure: last_success (the
		// health the steward reads) does not move, nor does any state.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	w.Runtime.KeelBranches = model.MustJSON(r.State)
	rep.Set, rep.Unset, rep.Forms, rep.Pings = r.Set, r.Unset, r.Forms, r.Pings
	rep.Logf("%s: round %d: %d keel call(s), %d branch(es), %d form write(s), %d ping(s), %d queued",
		w.Name, w.Runtime.Round, r.Calls, len(r.State.Branches), len(r.Forms), len(r.Pings), len(r.State.Queue))
	return rep, nil
}
