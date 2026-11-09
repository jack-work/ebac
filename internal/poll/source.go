package poll

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// MarkGone writes the gone marker for one resource of a critic: what is gone,
// since when, the last error, and what to do. Every source's plugin reports
// it the same way, so a seat reads the same shape whatever went away.
func (p *Poller) MarkGone(ctx context.Context, w *model.Critic, g plugin.Gone) error {
	marker := map[string]string{"resource": g.Resource, "since": g.Since, "error": g.Error, "action": g.Action}
	return p.Store.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		gone := model.ObjectOf(cur["gone"])
		gone[g.Key] = model.MustJSON(marker)
		return map[string]json.RawMessage{"gone": model.MustJSON(gone)}, nil
	})
}

// WakeOnce tells the critic's alert target one thing; a role with nobody
// seated is skipped.
func (p *Poller) WakeOnce(ctx context.Context, w *model.Critic, msg string) {
	target := w.AlertTarget()
	if !p.RoleHasHolder(ctx, target) {
		return
	}
	if err := p.Fig.Send(ctx, target, msg); err != nil {
		fmt.Fprintf(p.Out, "%s: could not deliver: %v\n", w.Name, err)
	}
}
