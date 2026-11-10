package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// The scout (model.SourceScout) is the one source whose plugin writes a form
// other than its critic's own: the ledger outbox on its inputs form. It also
// reads two outside forms through the core: the inputs form (the merge rule,
// ledger acks) and, read-only, the board's owner marks. Plugins cannot dial
// figaro, so the core does every such read and write, after checking the
// whole reply (ValidateForms), and only through form.Commit.

// InputsWriteKeys are the inputs-form keys a plugin may write.
var InputsWriteKeys = map[string]bool{"ledger_outbox": true}

// ValidateForms refuses a reply whose form writes reach outside what the
// source may write, naming the first offence. Nothing of the reply is applied
// when it fails.
func ValidateForms(w *model.Critic, rep *plugin.Reply) error {
	if len(rep.Forms) == 0 {
		return nil
	}
	if w.SourceName() != model.SourceScout {
		return fmt.Errorf("source %s may not write other forms (%d form writes): reply refused", w.SourceName(), len(rep.Forms))
	}
	for i, fw := range rep.Forms {
		where := fmt.Sprintf("form write %d", i)
		switch {
		case !fw.Inputs:
			return fmt.Errorf("%s names no target (want inputs): reply refused", where)
		case w.InputsForm == "":
			return fmt.Errorf("%s: the critic has no inputs form: reply refused", where)
		}
		for k := range fw.Set {
			if !InputsWriteKeys[k] {
				return fmt.Errorf("%s may not set inputs key %q (allowed: ledger_outbox): reply refused", where, k)
			}
		}
	}
	return nil
}

// commitOutbox merges a reply's ledger_outbox members into the inputs form's
// ledger_outbox object and writes it whole (a dotted key would be a flat decoy,
// jack-work/figaro#43): a member whose value is null is removed. Read, merged
// and written in one versioned Commit, so the steward's ledger_ack writes and
// the outbox never clobber each other.
func (p *Poller) commitOutbox(ctx context.Context, id string, set map[string]json.RawMessage) error {
	add := model.ObjectOf(set["ledger_outbox"])
	if len(add) == 0 {
		return nil
	}
	return p.Store.Commit(ctx, id, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		box := model.ObjectOf(cur["ledger_outbox"])
		for k, v := range add {
			if string(v) == "null" {
				delete(box, k)
			} else {
				box[k] = v
			}
		}
		next := model.MustJSON(box)
		if jsonEqual(cur["ledger_outbox"], next) {
			return nil, nil
		}
		return map[string]json.RawMessage{"ledger_outbox": next}, nil
	})
}

// landForms applies a validated reply's outbox writes, in order, then its
// pings. Its error fails the round after the critic is saved.
func (p *Poller) landForms(ctx context.Context, w *model.Critic, rep *plugin.Reply) error {
	if w.SourceName() != model.SourceScout {
		return nil
	}
	for _, fw := range rep.Forms {
		if err := p.commitOutbox(ctx, w.InputsForm, fw.Set); err != nil {
			return fmt.Errorf("%s: inputs form write: %w", w.Name, err)
		}
	}
	p.sendPings(ctx, w, rep.Pings)
	return nil
}

// MaxPings bounds one round's pings: a pass gone wrong must not flood a role.
const MaxPings = 10

var pingTo = regexp.MustCompile(`^@[0-9a-f]{8,32}$`)

// ValidatePings refuses a reply whose pings are not one short line to a role
// form, or more than MaxPings, or from a source other than the scout.
func ValidatePings(w *model.Critic, rep *plugin.Reply) error {
	if len(rep.Pings) == 0 {
		return nil
	}
	if w.SourceName() != model.SourceScout {
		return fmt.Errorf("source %s may not ping roles: reply refused", w.SourceName())
	}
	if len(rep.Pings) > MaxPings {
		return fmt.Errorf("%d pings in one round (max %d): reply refused", len(rep.Pings), MaxPings)
	}
	for i, pg := range rep.Pings {
		switch {
		case !pingTo.MatchString(pg.To):
			return fmt.Errorf("ping %d: to %q is not a role form @<hex>: reply refused", i, pg.To)
		case pg.Text == "" || len(pg.Text) > 400 || strings.ContainsAny(pg.Text, "\n\r"):
			return fmt.Errorf("ping %d: text must be one line of 1-400 bytes: reply refused", i)
		}
	}
	return nil
}

// sendPings delivers the pings to the roles' holders; a role nobody holds is
// skipped and said.
func (p *Poller) sendPings(ctx context.Context, w *model.Critic, pings []plugin.Ping) {
	send, holder := p.Send, p.Holder
	if send == nil {
		send = p.Fig.Send
	}
	if holder == nil {
		holder = p.RoleHasHolder
	}
	for _, pg := range pings {
		if !holder(ctx, pg.To) {
			fmt.Fprintf(p.Out, "%s: ping to %s skipped (no holder): %s\n", w.Name, pg.To, pg.Text)
			continue
		}
		if err := send(ctx, pg.To, pg.Text); err != nil {
			fmt.Fprintf(p.Out, "%s: ping to %s failed: %v\n", w.Name, pg.To, err)
		}
	}
}

// BoardReadKeys are the board form keys the scout reads, read-only: the
// owner marks the merge checklist needs (prs rows, then items).
var BoardReadKeys = []string{"prs", "items"}

// readBoard is the board form's BoardReadKeys.
func (p *Poller) readBoard(ctx context.Context, w *model.Critic) (map[string]json.RawMessage, error) {
	if w.BoardForm == "" {
		return nil, nil
	}
	f, err := p.Store.ReadForm(ctx, w.BoardForm)
	if err != nil {
		return nil, fmt.Errorf("board form %s: %w", w.BoardForm, err)
	}
	out := map[string]json.RawMessage{}
	for _, k := range BoardReadKeys {
		if v, ok := f.Keys[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// readInputs is the critic's inputs form for the plugin: every top-level key
// but system.
func (p *Poller) readInputs(ctx context.Context, w *model.Critic) (map[string]json.RawMessage, error) {
	if w.InputsForm == "" {
		return nil, nil
	}
	f, err := p.Store.ReadForm(ctx, w.InputsForm)
	if err != nil {
		return nil, fmt.Errorf("inputs form %s: %w", w.InputsForm, err)
	}
	out := map[string]json.RawMessage{}
	for k, v := range f.Keys {
		if k != "system" {
			out[k] = v
		}
	}
	return out, nil
}
