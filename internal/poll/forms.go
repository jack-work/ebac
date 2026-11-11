package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Two sources write forms other than their critic's own. The scout
// (model.SourceScout) writes the ledger outbox on its inputs form, and reads
// two outside forms through the core: the inputs form (the merge rule, ledger
// acks) and, read-only, the board's owner marks. Keel branches
// (model.SourceKeelBranches) write one child form per branch, minted by the
// core with a ChildLease lifetime, and read each child's holder-owned keys.
// Plugins cannot dial figaro, so the core does every such read and write,
// after checking the whole reply (ValidateForms), and only through
// form.Commit.

// InputsWriteKeys are the inputs-form keys a plugin may write.
var InputsWriteKeys = map[string]bool{"ledger_outbox": true}

var childKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// ValidateForms refuses a reply whose form writes reach outside what the
// source may write, naming the first offence. Nothing of the reply is applied
// when it fails.
func ValidateForms(w *model.Critic, rep *plugin.Reply) error {
	if len(rep.Forms) == 0 {
		return nil
	}
	src := w.SourceName()
	if src != model.SourceScout && src != model.SourceKeelBranches {
		return fmt.Errorf("source %s may not write other forms (%d form writes): reply refused", src, len(rep.Forms))
	}
	minted := map[string]bool{}
	for k := range w.Runtime.Children {
		minted[k] = true
	}
	for i, fw := range rep.Forms {
		where := fmt.Sprintf("form write %d", i)
		if src == model.SourceScout {
			switch {
			case !fw.Inputs || fw.Key != "" || fw.Retire || len(fw.Unset) > 0:
				return fmt.Errorf("%s: the scout writes only its inputs form's outbox: reply refused", where)
			case w.InputsForm == "":
				return fmt.Errorf("%s: the critic has no inputs form: reply refused", where)
			}
			for k := range fw.Set {
				if !InputsWriteKeys[k] {
					return fmt.Errorf("%s may not set inputs key %q (allowed: ledger_outbox): reply refused", where, k)
				}
			}
			continue
		}
		switch {
		case fw.Inputs || fw.Key == "":
			return fmt.Errorf("%s: keel branches write only child forms (want a key): reply refused", where)
		case !childKey.MatchString(fw.Key):
			return fmt.Errorf("%s: child key %q is not [a-z0-9-], 1-100: reply refused", where, fw.Key)
		}
		check := func(verb, k string) error {
			switch {
			case k == "" || strings.ContainsAny(k, ".*"):
				return fmt.Errorf("%s may not %s %q (top-level keys only): reply refused", where, verb, k)
			case model.ChildWriteRefused[k]:
				return fmt.Errorf("%s may not %s %q on child %s (owner or core key): reply refused", where, verb, k, fw.Key)
			}
			return nil
		}
		for k := range fw.Set {
			if err := check("set", k); err != nil {
				return err
			}
		}
		for _, k := range fw.Unset {
			if err := check("unset", k); err != nil {
				return err
			}
		}
		if fw.Retire {
			delete(minted, fw.Key)
		} else if len(fw.Set) > 0 {
			minted[fw.Key] = true
		}
	}
	if len(minted) > model.MaxChildren {
		return fmt.Errorf("%d child forms after this round (max %d): reply refused", len(minted), model.MaxChildren)
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

// landForms applies a validated reply's form writes, in order, renews the
// children's leases, then sends its pings. It stops at the first failed
// write: what landed stays recorded in runtime.children, so nothing is
// orphaned. Its error fails the round after the critic is saved.
func (p *Poller) landForms(ctx context.Context, w *model.Critic, rep *plugin.Reply) error {
	src := w.SourceName()
	if src != model.SourceScout && src != model.SourceKeelBranches {
		return nil
	}
	var err error
	for _, fw := range rep.Forms {
		if fw.Inputs {
			err = p.commitOutbox(ctx, w.InputsForm, fw.Set)
		} else {
			err = p.writeChild(ctx, w, fw)
		}
		if err != nil {
			err = fmt.Errorf("%s: form write %s: %w", w.Name, describe(fw), err)
			break
		}
	}
	p.renewChildren(ctx, w)
	if err == nil {
		p.sendPings(ctx, w, rep.Pings)
	}
	return err
}

func describe(fw plugin.FormWrite) string {
	if fw.Inputs {
		return "inputs"
	}
	return "child " + fw.Key
}

// childKind marks a child form for anyone listing forms.
const childKind = "ebac.child"

func childName(w *model.Critic, key string) string { return "ebac-" + w.Name + "-" + key }

// commitForm writes set and unset on a form through form.Commit, only the keys
// whose value changed.
func (p *Poller) commitForm(ctx context.Context, id string, set map[string]json.RawMessage, unset []string) error {
	if len(set) == 0 && len(unset) == 0 {
		return nil
	}
	return p.Store.Commit(ctx, id, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		out := map[string]json.RawMessage{}
		for k, v := range set {
			if !jsonEqual(cur[k], v) {
				out[k] = v
			}
		}
		var gone []string
		for _, k := range unset {
			if _, ok := cur[k]; ok {
				gone = append(gone, k)
			}
		}
		return out, gone
	})
}

// writeChild lands one child write. A child is minted on its first write (name
// ebac-<critic>-<key>, system.ttl ChildLease) and kept in runtime.children; a
// child whose form is gone (lapsed or removed) is minted again. A retire
// renames it ebac-archived-<critic>-<key> and stops renewing it, so its lease
// runs out: never a kill.
func (p *Poller) writeChild(ctx context.Context, w *model.Critic, fw plugin.FormWrite) error {
	id := w.Runtime.Children[fw.Key]
	if id == "" && fw.Retire {
		return nil // never minted: nothing to retire
	}
	for attempt := 0; ; attempt++ {
		if id == "" {
			var err error
			if id, err = p.Fig.FormNewLease(ctx, childName(w, fw.Key), model.ChildLease); err != nil {
				return fmt.Errorf("mint: %w", err)
			}
			if w.Runtime.Children == nil {
				w.Runtime.Children = map[string]string{}
			}
			w.Runtime.Children[fw.Key] = id
			if err := p.commitForm(ctx, id, map[string]json.RawMessage{"kind": model.MustJSON(childKind),
				"parent_critic": model.MustJSON(w.Name), "parent_form": model.MustJSON(w.FormID)}, nil); err != nil {
				return err
			}
		}
		set := fw.Set
		if fw.Retire {
			set = map[string]json.RawMessage{}
			for k, v := range fw.Set {
				set[k] = v
			}
			set["name"] = model.MustJSON(model.ArchivedFormPrefix + w.Name + "-" + fw.Key)
		}
		err := p.commitForm(ctx, id, set, fw.Unset)
		if errors.Is(err, figaro.ErrFormGone) && attempt == 0 && !fw.Retire {
			id = "" // the child lapsed or was removed: mint it again
			continue
		}
		if err != nil {
			return err
		}
		if fw.Retire {
			delete(w.Runtime.Children, fw.Key)
		}
		return nil
	}
}

// renewChildren keeps every live child's lease ChildLease long, with one list
// of the store for all of them.
func (p *Poller) renewChildren(ctx context.Context, w *model.Critic) {
	if len(w.Runtime.Children) == 0 {
		return
	}
	ids := make([]string, 0, len(w.Runtime.Children))
	for _, id := range w.Runtime.Children {
		ids = append(ids, id)
	}
	if err := p.Fig.RenewLeases(ctx, ids, model.ChildLease); err != nil {
		fmt.Fprintf(p.Out, "%s: child leases: %v\n", w.Name, err)
	}
}

// readOwned is each child's holder-owned keys (model.ChildOwnerKeys) and its
// target-aria. An unreadable child is skipped: its owner keys are not news
// this round.
func (p *Poller) readOwned(ctx context.Context, w *model.Critic) map[string]map[string]json.RawMessage {
	out := map[string]map[string]json.RawMessage{}
	for key, id := range w.Runtime.Children {
		f, err := p.Store.ReadForm(ctx, id)
		if err != nil {
			continue
		}
		m := map[string]json.RawMessage{}
		for _, k := range append([]string{"target-aria"}, model.ChildOwnerKeys...) {
			if v, ok := f.Keys[k]; ok {
				m[k] = v
			}
		}
		out[key] = m
	}
	return out
}

// MaxPings bounds one round's pings: a pass gone wrong must not flood a role.
const MaxPings = 10

var pingTo = regexp.MustCompile(`^@[0-9a-f]{8,32}$`)

// ValidatePings refuses a reply whose pings are not one short line to a role
// form, or more than MaxPings, or from a source other than the scout and keel
// branches.
func ValidatePings(w *model.Critic, rep *plugin.Reply) error {
	if len(rep.Pings) == 0 {
		return nil
	}
	if src := w.SourceName(); src != model.SourceScout && src != model.SourceKeelBranches {
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
