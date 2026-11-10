// Package rosterstate derives the figaro family's role registry from the
// aria store: roles and their holders, lieutenants (a role whose holder was
// forked from another role's holder) and live transient forks. It is the pure
// half of the ebac-roster plugin: no I/O, so a fixture drives it whole.
package rosterstate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// ForksEvery bounds how often the forks key is rewritten: fork births and
// deaths are constant, and every write reaches every student (cheroot
// 2026-11-10). The full list stays in figaro ls.
const ForksEvery = 10 * time.Minute

// Dropped are keys earlier versions wrote; the first round unsets them once.
var Dropped = []string{"updated"}

// View is the form the roster writes: one key per role (role:<name>) and
// forks, one line of live transient fork COUNTS per parent role.
type View struct {
	Roles map[string]string // form key -> one line
	Forks string
}

// State is what the plugin wrote last round, so a round writes only change.
// Slim says the Dropped keys have been unset; ForksAt is when forks was last
// written.
type State struct {
	Keys    map[string]json.RawMessage `json:"keys"`
	Slim    bool                       `json:"slim,omitempty"`
	ForksAt time.Time                  `json:"forks_at,omitempty"`
}

// KeyFor is a role form's key. Dots and stars are escaped: a dot would nest
// the value under another key, and the core's allowlist refuses both.
func KeyFor(name string) string {
	return "role:" + strings.NewReplacer(".", "%2E", "*", "%2A").Replace(name)
}

func short(id string) string {
	id = strings.TrimPrefix(id, "@")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// index resolves an aria id, full or a prefix (roles and parents are often
// stored short), to a conversation.
type index struct {
	byID map[string]*model.Aria
	all  []*model.Aria
}

func newIndex(arias []model.Aria) *index {
	ix := &index{byID: map[string]*model.Aria{}}
	for i := range arias {
		a := &arias[i]
		if a.Kind != "" && a.Kind != "conversation" {
			continue
		}
		ix.byID[a.ID] = a
		ix.all = append(ix.all, a)
	}
	return ix
}

func (ix *index) get(id string) *model.Aria {
	if id == "" {
		return nil
	}
	if a := ix.byID[id]; a != nil {
		return a
	}
	var hit *model.Aria
	for _, a := range ix.all {
		if strings.HasPrefix(a.ID, id) {
			if hit != nil {
				return nil // ambiguous: say nothing rather than guess
			}
			hit = a
		}
	}
	return hit
}

// Derive is the roster for one listing of the store at now. Every line names
// only WHO (roles, holders, forks and what they hang under), never how a seat
// is doing (model, context fill, asleep, expiry): a studied form is mirrored
// whole on every write, so a line that moves with the seat's work rewrites
// every student's copy (ebac-leader 2026-11-10).
func Derive(arias []model.Aria, now time.Time) View {
	ix := newIndex(arias)
	type role struct {
		name, form string
		holder     *model.Aria
		target     string
	}
	var roles []role
	holds := map[string][]string{} // holder aria id -> role names
	for _, a := range arias {
		if a.Kind != "form" || a.TargetAria == "" {
			continue
		}
		name := a.Name
		if name == "" {
			name = short(a.ID)
		}
		r := role{name: name, form: a.ID, holder: ix.get(a.TargetAria), target: a.TargetAria}
		roles = append(roles, r)
		if r.holder != nil {
			holds[r.holder.ID] = append(holds[r.holder.ID], name)
		}
	}
	for id := range holds {
		sort.Strings(holds[id])
	}
	// leaderOf names the roles held by an aria's fork source (a seat may
	// hold several; naming one would guess), excluding the aria's own role.
	leaderOf := func(a *model.Aria, self string) string {
		if a == nil {
			return ""
		}
		p := ix.get(a.Parent)
		if p == nil {
			return ""
		}
		var names []string
		for _, n := range holds[p.ID] {
			if n != self {
				names = append(names, n)
			}
		}
		return strings.Join(names, "/")
	}

	v := View{Roles: map[string]string{}}
	for _, r := range roles {
		var b strings.Builder
		fmt.Fprintf(&b, "@%s -> ", short(r.form))
		if r.holder == nil {
			fmt.Fprintf(&b, "%s (not in the store)", short(r.target))
		} else {
			h := r.holder
			fmt.Fprintf(&b, "%s %s", short(h.ID), h.Handle)
			if l := leaderOf(h, r.name); l != "" {
				fmt.Fprintf(&b, " under %s", l)
			}
		}
		v.Roles[KeyFor(r.name)] = b.String()
	}

	nowMS := now.UnixMilli()
	var forks []*model.Aria
	for _, a := range ix.all {
		if a.TTLMS > 0 && a.ExpiresAt > nowMS && len(holds[a.ID]) == 0 {
			forks = append(forks, a)
		}
	}
	// Counts per parent role (a parent holding several names them all, as
	// one); a fork whose parent holds no role is "other".
	counts := map[string]int{}
	other := 0
	for _, a := range forks {
		p := ix.get(a.Parent)
		if p == nil || len(holds[p.ID]) == 0 {
			other++
			continue
		}
		counts[strings.Join(holds[p.ID], "/")]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names)+1)
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s %d", n, counts[n]))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("other %d", other))
	}
	v.Forks = strings.Join(parts, ", ")
	if v.Forks == "" {
		v.Forks = "none"
	}
	return v
}

// Keys is the view as form keys.
func (v View) Keys() map[string]json.RawMessage {
	out := map[string]json.RawMessage{"forks": model.MustJSON(v.Forks)}
	for k, line := range v.Roles {
		out[k] = model.MustJSON(line)
	}
	return out
}

// Step compares a view with what was written: set holds only changed keys,
// unset the keys whose role is gone (and, once, the Dropped keys). forks is
// rewritten at most every ForksEvery (at once over a pre-count list).
func Step(prev State, v View, now time.Time) (set map[string]json.RawMessage, unset []string, next State) {
	keys := v.Keys()
	forksAt := prev.ForksAt
	if old, ok := prev.Keys["forks"]; ok && string(old) != string(keys["forks"]) {
		if !strings.HasPrefix(string(old), "[") && now.Sub(prev.ForksAt) < ForksEvery {
			keys["forks"] = old // hold: what the form shows until the window passes
		} else {
			forksAt = now.UTC()
		}
	} else if !ok {
		forksAt = now.UTC()
	}
	set = map[string]json.RawMessage{}
	for k, val := range keys {
		if string(prev.Keys[k]) != string(val) {
			set[k] = val
		}
	}
	for k := range prev.Keys {
		if _, ok := keys[k]; !ok {
			unset = append(unset, k)
		}
	}
	if !prev.Slim {
		unset = append(unset, Dropped...)
	}
	sort.Strings(unset)
	return set, unset, State{Keys: keys, Slim: true, ForksAt: forksAt}
}
