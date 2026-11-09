package bandstate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Plugin is ebac-bandhub: one round reads the hub and returns the mirror's
// form keys. It has no seat and no events: Done ends every round, so the core
// writes the keys and delivers nothing.
type Plugin struct {
	// Hub, Tip and Now replace the socket client, the forge and the clock in
	// tests; nil means the real ones.
	Hub *Hub
	Tip TipFunc
	Now func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: a band-hub mirror has no seat, no brief and no outbox", req.Op)
	}
	w := &req.Critic
	if w.Band == nil {
		return nil, fmt.Errorf("%s: band-hub mirror without its configuration", w.Name)
	}
	cfg := Config{Socket: w.Band.Socket, Utility: w.Band.Utility, Repo: w.Band.Repo}
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	rep := &plugin.Reply{Critic: w, Done: true}

	h := pl.Hub
	if h == nil {
		var err error
		if h, err = (&Client{Socket: cfg.Socket}).Fetch(ctx); err != nil {
			w.Runtime.ConsecutiveFailures++
			w.Runtime.LastError = err.Error()
			rep.Error = err.Error()
			return rep, nil
		}
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()

	var prev State
	if len(w.Runtime.BandHub) > 0 {
		if err := json.Unmarshal(w.Runtime.BandHub, &prev); err != nil {
			rep.Logf("%s: unreadable state, starting over: %v", w.Name, err)
			prev = State{}
		}
	}
	tip := pl.Tip
	if tip == nil {
		gh := forge.NewGH(req.GH, req.Host)
		if w.Host != "" {
			gh.Host = w.Host // the token follows the host
		}
		tip = gh.BranchTip
	}
	r := Step(ctx, prev, h, cfg, now(), tip)
	w.Runtime.BandHub = model.MustJSON(r.State)
	rep.Set = r.Set
	if r.State.TipErr != "" {
		rep.Logf("%s: branch tips: %s (keeping the last ones)", w.Name, r.State.TipErr)
	}
	if len(r.Changed) > 0 {
		rep.Logf("%s: round %d wrote %v", w.Name, w.Runtime.Round, r.Changed)
	} else {
		rep.Logf("%s: round %d unchanged", w.Name, w.Runtime.Round)
	}
	return rep, nil
}
