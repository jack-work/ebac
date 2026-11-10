package rosterstate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gluck/ebac/internal/plugin"
)

// Plugin is ebac-roster: one round derives the roster from the aria list the
// core passed in (Request.Arias) and returns only the keys that changed. No
// seat, no events, no brief: Done ends every round.
type Plugin struct {
	// Now replaces the clock in tests.
	Now func() time.Time
}

func (pl Plugin) Handle(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: the roster has no seat, no brief and no outbox", req.Op)
	}
	w := &req.Critic
	if len(req.Arias) == 0 {
		// An empty store is not news; it is a core that read nothing.
		// Keep the last roster rather than unset every role.
		return nil, fmt.Errorf("%s: the core passed no arias", w.Name)
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	var prev State
	if len(w.Runtime.Roster) > 0 {
		if err := json.Unmarshal(w.Runtime.Roster, &prev); err != nil {
			prev = State{}
		}
	}
	set, unset, next := Step(prev, Derive(req.Arias, now()))
	w.Runtime.Roster = mustJSON(next)
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	rep := &plugin.Reply{Critic: w, Done: true, Unset: unset}
	if len(set) > 0 {
		rep.Set = set
	}
	if len(set) > 0 || len(unset) > 0 {
		rep.Logf("%s: round %d wrote %d key(s), removed %d", w.Name, w.Runtime.Round, len(set), len(unset))
	} else {
		rep.Logf("%s: round %d unchanged", w.Name, w.Runtime.Round)
	}
	return rep, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
