package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// RolePaths are every role-form address a critic's configuration (its critic
// key) stores, as paths into that JSON ("[]" walks a list). The critic's own
// role form and its seat (that form's target-aria) are not here: the seat is
// ebac cast's. The board and inputs forms are data forms, not roles.
// TestRolePathsCoverTheModel fails when a field holding an @form is added to
// the model and not listed here (or in NotRolePaths).
var RolePaths = []string{
	"author_watch.alert_role",
	"keel.alert_role",
	"releases_watch.alert_role",
	"scout_watch.owners[].role",
	"scout_watch.warn_to",
	"keel_branches.reviewer",
}

// AlertPaths are the alert-role fields (ebac repoint --critic N --alert @new).
var AlertPaths = []string{"author_watch.alert_role", "keel.alert_role", "releases_watch.alert_role"}

// NotRolePaths are the @-valued configuration fields that are not role
// addresses.
var NotRolePaths = []string{"board_form", "inputs_form"}

// Change is one address rewritten (or, on a dry run, to be).
type Change struct {
	Critic string `json:"critic"`
	Field  string `json:"field"`
	Old    string `json:"old"`
	New    string `json:"new"`
}

// RepointReq is one repoint: Critic + Alert sets that critic's alert role;
// From + To rewrites every role address equal to From, in every critic.
type RepointReq struct {
	Critic, Alert string
	From, To      string
	DryRun        bool
}

// Forms resolves @ids against the store: a full id or a unique prefix of a
// form's id.
type Forms []model.Aria

func (fs Forms) resolve(ref string) (model.Aria, error) {
	id := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "@"))
	if len(id) < 4 {
		return model.Aria{}, fmt.Errorf("%q: want @<form id> or a unique prefix of one", ref)
	}
	var hits []model.Aria
	for _, a := range fs {
		if a.Kind != "form" {
			continue
		}
		if a.ID == "@"+id || strings.TrimPrefix(a.ID, "@") == id {
			return a, nil
		}
		if strings.HasPrefix(strings.TrimPrefix(a.ID, "@"), id) {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 0:
		return model.Aria{}, fmt.Errorf("%s: no such form", ref)
	case 1:
		return hits[0], nil
	}
	return model.Aria{}, fmt.Errorf("%s: %d forms share that prefix", ref, len(hits))
}

func fullID(a model.Aria) string { return "@" + strings.TrimPrefix(a.ID, "@") }

// sameForm says whether a stored address names the form full (@<32 hex>): the
// full id, or a prefix of it of at least 8 hex (what older configs stored).
func sameForm(stored, full string) bool {
	s := strings.ToLower(strings.TrimPrefix(stored, "@"))
	f := strings.TrimPrefix(full, "@")
	return s == f || len(s) >= 8 && strings.HasPrefix(f, s)
}

// Repoint rewrites role addresses in critic keys, through the store's Commit
// guard, each critic key rewritten whole (every other field kept as it is).
// It refuses a target that does not resolve to a role form with a holder
// (target-aria). It is idempotent: a second run finds nothing to change.
func Repoint(ctx context.Context, st *form.Store, forms Forms, req RepointReq) ([]Change, error) {
	alert := req.Critic != "" || req.Alert != ""
	all := req.From != "" || req.To != ""
	switch {
	case alert == all:
		return nil, fmt.Errorf("give either --critic N --alert @new or --all-from @old --to @new")
	case alert && (req.Critic == "" || req.Alert == ""):
		return nil, fmt.Errorf("--critic and --alert go together")
	case all && (req.From == "" || req.To == ""):
		return nil, fmt.Errorf("--all-from and --to go together")
	}
	target := req.Alert
	if all {
		target = req.To
	}
	to, err := forms.resolve(target)
	if err != nil {
		return nil, fmt.Errorf("new role: %w", err)
	}
	if to.TargetAria == "" {
		return nil, fmt.Errorf("new role %s is not a role form with a holder (no target-aria): refused", fullID(to))
	}
	newID := fullID(to)
	var oldID string
	if all {
		from, err := forms.resolve(req.From)
		if err != nil {
			return nil, fmt.Errorf("old role: %w", err)
		}
		if oldID = fullID(from); oldID == newID {
			return nil, fmt.Errorf("--all-from and --to are the same form %s", oldID)
		}
	}

	var critics []*model.Critic
	if alert {
		w, err := st.LoadCritic(ctx, req.Critic)
		if err != nil {
			return nil, err
		}
		critics = []*model.Critic{w}
	} else if critics, err = st.ListCritics(ctx); err != nil {
		return nil, err
	}

	var out []Change
	for _, w := range critics {
		pick := func(path, val string) bool { return sameForm(val, oldID) }
		paths := RolePaths
		if alert {
			paths = AlertPaths
			pick = func(path, val string) bool { return val != newID }
		}
		f, err := st.ReadForm(ctx, w.FormID)
		if err != nil {
			return out, fmt.Errorf("%s: %w", w.Name, err)
		}
		_, changes, found := rewrite(f.Keys[model.CriticKey], paths, pick, newID)
		if alert && !found {
			return out, fmt.Errorf("critic %s (source %s) has no alert role: its wake goes to its own role form's holder, which is ebac cast's (ebac cast --critic %s --aria <id>)",
				w.Name, w.SourceName(), w.Name)
		}
		if len(changes) == 0 {
			continue
		}
		for i := range changes {
			changes[i].Critic = w.Name
		}
		if !req.DryRun {
			var landed []Change
			err := st.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
				next, c, _ := rewrite(cur[model.CriticKey], paths, pick, newID)
				landed = c
				if len(c) == 0 {
					return nil, nil
				}
				return map[string]json.RawMessage{model.CriticKey: next}, nil
			})
			if err != nil {
				return out, fmt.Errorf("%s: %w", w.Name, err)
			}
			for i := range landed {
				landed[i].Critic = w.Name
			}
			changes = landed
		}
		out = append(out, changes...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Critic != out[j].Critic {
			return out[i].Critic < out[j].Critic
		}
		return out[i].Field < out[j].Field
	})
	return out, nil
}

// rewrite sets every string at paths for which pick says yes to to, in a
// critic key. It returns the key (whole, everything else kept), the changes,
// and whether any of the paths exists at all.
func rewrite(raw json.RawMessage, paths []string, pick func(path, val string) bool, to string) (json.RawMessage, []Change, bool) {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return raw, nil, false
	}
	var changes []Change
	found := false
	for _, p := range paths {
		walk(root, strings.Split(p, "."), p, "", func(field string, get func() string, set func(string)) {
			found = true
			if v := get(); pick(p, v) {
				changes = append(changes, Change{Field: field, Old: v, New: to})
				set(to)
			}
		})
	}
	if len(changes) == 0 {
		return raw, nil, found
	}
	return model.MustJSON(root), changes, found
}

// walk visits each string reached by segs from node ("x[]" walks the list at
// x), naming it field (list items by index), with its parent object present.
func walk(node any, segs []string, path, field string, visit func(field string, get func() string, set func(string))) {
	m, ok := node.(map[string]any)
	if !ok || len(segs) == 0 {
		return
	}
	seg := segs[0]
	name, list := strings.CutSuffix(seg, "[]")
	at := name
	if field != "" {
		at = field + "." + name
	}
	if list {
		items, _ := m[name].([]any)
		for i, it := range items {
			walk(it, segs[1:], path, fmt.Sprintf("%s[%d]", at, i), visit)
		}
		return
	}
	if len(segs) == 1 {
		visit(at, func() string { s, _ := m[name].(string); return s }, func(v string) { m[name] = v })
		return
	}
	walk(m[name], segs[1:], path, at, visit)
}

// ListForms is the form store for Repoint.
func ListForms(ctx context.Context, fig *figaro.Figaro) (Forms, error) {
	arias, err := fig.ListArias(ctx)
	if err != nil {
		return nil, err
	}
	var out Forms
	for _, a := range arias {
		if a.Kind == "form" {
			out = append(out, a)
		}
	}
	return out, nil
}
