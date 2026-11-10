package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Board sync (model.SourceBoard) is the one source whose plugin writes forms
// other than its critic's own: one child form per PR, the board form, and the
// inputs form's ledger outbox. Plugins cannot dial figaro, so the core does
// every such write, after checking the whole reply (ValidateForms), and only
// through form.Commit (the critic-key guard applies to every write).

var childKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidateForms refuses a reply whose form writes reach outside what the
// source may write, naming the first offence. Nothing of the reply is applied
// when it fails.
func ValidateForms(w *model.Critic, rep *plugin.Reply) error {
	if len(rep.Forms) == 0 {
		return nil
	}
	if w.SourceName() != model.SourceBoard {
		return fmt.Errorf("source %s may not write other forms (%d form writes): reply refused", w.SourceName(), len(rep.Forms))
	}
	for i, fw := range rep.Forms {
		targets := 0
		for _, t := range []bool{fw.Key != "", fw.Board, fw.Inputs} {
			if t {
				targets++
			}
		}
		where := fmt.Sprintf("form write %d", i)
		switch {
		case targets != 1:
			return fmt.Errorf("%s names %d targets (want exactly one of key, board, inputs): reply refused", where, targets)
		case fw.Key != "" && !childKey.MatchString(fw.Key):
			return fmt.Errorf("%s: child key %q is not [a-z0-9-]: reply refused", where, fw.Key)
		case fw.Board && w.BoardForm == "":
			return fmt.Errorf("%s: the critic has no board form: reply refused", where)
		case fw.Inputs && w.InputsForm == "":
			return fmt.Errorf("%s: the critic has no inputs form: reply refused", where)
		case fw.Retire && fw.Key == "":
			return fmt.Errorf("%s: only a child form retires: reply refused", where)
		}
		check := func(verb, k string) error {
			switch {
			case k == "" || strings.ContainsAny(k, ".*"):
				return fmt.Errorf("%s may not %s %q (top-level keys only): reply refused", where, verb, k)
			case model.FormWriteRefused[k]:
				return fmt.Errorf("%s may not %s %q (owner or core key): reply refused", where, verb, k)
			case fw.Board && !model.BoardFormKeys[k]:
				return fmt.Errorf("%s may not %s board key %q (allowed: %s): reply refused", where, verb, k, keyList(model.BoardFormKeys))
			case fw.Inputs && !model.InputsFormKeys[k]:
				return fmt.Errorf("%s may not %s inputs key %q (allowed: %s): reply refused", where, verb, k, keyList(model.InputsFormKeys))
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
	}
	return nil
}

func keyList(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// childKind marks a child form for anyone listing forms.
const childKind = "ebac.child"

// applyForms lands a validated reply's form writes, in order. A child form is
// minted on its first write (name ebac-<critic>-<key>, lease ChildLease) and
// kept in runtime.children; a child whose form is gone is minted again. Every
// write goes through form.Commit. It stops at the first failure: what landed
// stays recorded in runtime.children, so nothing is orphaned.
func (p *Poller) applyForms(ctx context.Context, w *model.Critic, writes []plugin.FormWrite) error {
	for _, fw := range writes {
		var err error
		switch {
		case fw.Board:
			err = p.commitForm(ctx, w.BoardForm, fw.Set, fw.Unset)
		case fw.Inputs:
			err = p.commitOutbox(ctx, w.InputsForm, fw.Set)
		default:
			err = p.writeChild(ctx, w, fw)
		}
		if err != nil {
			return fmt.Errorf("%s: form write %s: %w", w.Name, describe(fw), err)
		}
	}
	return nil
}

func describe(fw plugin.FormWrite) string {
	switch {
	case fw.Board:
		return "board"
	case fw.Inputs:
		return "inputs"
	}
	return "child " + fw.Key
}

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

func childName(w *model.Critic, key string) string { return "ebac-" + w.Name + "-" + key }

func (p *Poller) writeChild(ctx context.Context, w *model.Critic, fw plugin.FormWrite) error {
	id := w.Runtime.Children[fw.Key]
	if id == "" && fw.Retire && len(fw.Set) == 0 {
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
			delete(w.Runtime.Children, fw.Key) // no longer renewed: its lease runs out
		}
		return nil
	}
}

// landForms applies a reply's form writes and renews the children's leases.
// Its error fails the round after the critic (with runtime.children) is saved.
func (p *Poller) landForms(ctx context.Context, w *model.Critic, rep *plugin.Reply) error {
	if w.SourceName() != model.SourceBoard {
		return nil
	}
	err := p.applyForms(ctx, w, rep.Forms)
	p.renewChildren(ctx, w)
	return err
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
