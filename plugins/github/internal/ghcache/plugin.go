package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// Plugin serves github-cache. GH and Now replace the forge and the clock in
// tests; nil means the real ones.
type Plugin struct {
	GH  GitHub
	Now func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: the GitHub cache has no brief and no outbox", req.Op)
	}
	w := &req.Critic
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	gh := pl.GH
	if gh == nil {
		g := forge.NewGH(req.GH, req.Host)
		if w.Host != "" {
			g.Host = w.Host
		}
		gh = g
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	fail := func(err error) (*plugin.Reply, error) {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	cfg, ttl, err := ParseConfig(req.Keys[model.GHCacheConfigKey])
	if err != nil {
		return fail(err)
	}
	var prev State
	if len(w.Runtime.GHCache) > 0 {
		if err := json.Unmarshal(w.Runtime.GHCache, &prev); err != nil {
			prev = State{}
		}
	}
	r, err := Run(ctx, gh, prev, Pass{Config: cfg, ClosedTTL: ttl, Now: now(), Children: w.Runtime.Children, Landed: w.Runtime.FormsLanded})
	for _, l := range r.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if sp, ok := gh.(interface{ Spent() forge.Spend }); ok {
		rep.Logf("%s: cost: %s", w.Name, sp.Spent()) // also when the round failed
		rep.Spend = sp.Spent().Budget()
	}
	if err != nil {
		return fail(err)
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	w.Runtime.GHCache = model.MustJSON(r.State)
	rep.Forms = r.Forms
	rep.SetKey("track", r.Track)
	return rep, nil
}
