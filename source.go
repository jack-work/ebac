package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gluck/ebac/internal/model"
)

// A Source translates one upstream (GitHub pull requests, a Teams chat, later
// ADO pipelines or mail) into a critic's role form. Each source owns its
// fetch, its diff, its inbox items and its wake-ups; the core owns the form,
// its budget and lease, the ack, the outbox and the timers. Each critic has
// its own timer and process, so one slow upstream never delays another.
type Source interface {
	Name() string
	Round(ctx context.Context, p *Poller, w *model.Critic) error
}

var sources = map[string]Source{}

// Register makes a source reachable by its Name. The binary registers every
// source at start-up, before any round runs.
func Register(s Source) { sources[s.Name()] = s }

// markGone writes the gone marker for one resource of a critic: what is gone,
// since when, the last error, and what to do. Every source uses it, so a seat
// reads the same shape whatever went away.
func (p *Poller) markGone(ctx context.Context, w *model.Critic, key, resource, since string, cause error, action string) error {
	marker := map[string]string{"resource": resource, "since": since, "error": cause.Error(), "action": action}
	return p.Store.commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ ebacState) (map[string]json.RawMessage, []string) {
		gone := model.ObjectOf(cur["gone"])
		gone[key] = model.MustJSON(marker)
		return map[string]json.RawMessage{"gone": model.MustJSON(gone)}, nil
	})
}

func (p *Poller) wakeOnce(ctx context.Context, w *model.Critic, msg string) {
	if !p.roleHasHolder(ctx, w.FormID) {
		return
	}
	if err := p.Fig.Send(ctx, w.FormID, msg); err != nil {
		fmt.Fprintf(p.Out, "%s: could not deliver: %v\n", w.Name, err)
	}
}
