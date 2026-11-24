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

// praxisNew reads ours.praxis_new from the register's ours key.
func praxisNew(ours json.RawMessage) map[string]json.RawMessage {
	var o struct {
		PraxisNew map[string]json.RawMessage `json:"praxis_new"`
	}
	if len(ours) > 0 {
		_ = json.Unmarshal(ours, &o)
	}
	return o.PraxisNew
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
	pass := Pass{Config: cfg, ClosedTTL: ttl, Now: now(), Children: w.Runtime.Children, Landed: w.Runtime.FormsLanded,
		Owned: req.Owned, Critic: w.Name,
		// Every write is authorized against the trusted scope the core
		// applied to this critic (model.GHWrite), never the form's config.
		Auth:       func(e model.GHEntity, field string) string { return model.AuthorizeGHWrite(w, e, field) },
		Maintainer: w.Maintainer, PraxisNew: praxisNew(req.Keys[model.GHCacheOursKey]), Wake: wakeSet(req.Wake)}
	if w.GHWrite != nil {
		pass.Ours = w.GHWrite.Ours
	}
	r, err := Run(ctx, gh, prev, pass)
	for _, l := range r.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if sp, ok := gh.(interface{ Spent() forge.Spend }); ok {
		s := sp.Spent()
		rep.Logf("%s: cost: %s", w.Name, s) // also when the round failed
		rep.Spend = s.Budget()
		r.Telemetry.GraphQL = &Cost{Calls: s.Calls, Points: s.Points, Left: s.Remaining}
		r.Telemetry.RESTCalls = s.All - s.Calls
	}
	if err != nil {
		r.Telemetry.Error = cut(err.Error(), 300)
	}
	if r.Telemetry.At == "" {
		r.Telemetry.At = now().UTC().Format(time.RFC3339)
	}
	r.Telemetry.metrics()
	rep.Telemetry = model.MustJSON(r.Telemetry)
	if err != nil {
		return fail(err)
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	w.Runtime.GHCache = model.MustJSON(r.State)
	rep.Forms = r.Forms
	rep.Pings = r.Pings
	rep.SetKey("track", r.Track)
	if r.Register != nil {
		rep.SetKey(model.GHCacheSyncKey, r.Register) // only when it changed (State.RegHash)
	}
	return rep, nil
}

func wakeSet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}
