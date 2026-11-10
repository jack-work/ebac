package poll

import (
	"context"
	"encoding/json"
	"errors"
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

// surfaceBroken turns "no critic named N" into the truth when N's form still
// exists but its critic key is unusable: the form gets critic_error, its
// holder is woken ONCE (when critic_error changes), and the round fails
// naming the form. Never a silent drop (2026-11-10: four Teams critics with
// critic: {} vanished from ebac ls and a Boss DM went unseen for an hour).
func (p *Poller) surfaceBroken(ctx context.Context, name string) error {
	broken, err := p.Store.BrokenCritics(ctx)
	if err != nil {
		return nil
	}
	for _, b := range broken {
		if b.Critic() != name {
			continue
		}
		msg := fmt.Sprintf("ebac %s: form %s holds an unusable critic key (%s), so the critic cannot run and nothing reaches you. "+
			"Restore the critic key, or recreate the critic (ebac add-...); this form keeps its inbox and runtime.", name, b.FormID, b.Err)
		if b.Noted != msg {
			if err := p.Store.Commit(ctx, b.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
				return map[string]json.RawMessage{model.CriticErrorKey: model.MustJSON(msg)}, nil
			}); err != nil {
				fmt.Fprintf(p.Out, "%s: could not write %s on %s: %v\n", name, model.CriticErrorKey, b.FormID, err)
			} else if p.RoleHasHolder(ctx, b.FormID) {
				if err := p.Fig.Send(ctx, b.FormID, msg); err != nil {
					fmt.Fprintf(p.Out, "%s: could not wake the holder of %s: %v\n", name, b.FormID, err)
				}
			}
		}
		return errors.New(msg)
	}
	return nil
}
