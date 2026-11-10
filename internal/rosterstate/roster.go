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

// ForksMax caps the forks key, newest first: past it the list ends in
// "+N more (figaro ls)", so the value stays small and under figaro's
// 2046-character cut.
const ForksMax = 20

// Dropped are keys earlier versions wrote; the first round unsets them once.
var Dropped = []string{"updated"}

// View is the form the roster writes: one key per role (role:<name>) and
// the forks list.
type View struct {
	Roles map[string]string // form key -> one line
	Forks []string
}

// State is what the plugin wrote last round, so a round writes only change.
// Slim says the Dropped keys have been unset.
type State struct {
	Keys map[string]json.RawMessage `json:"keys"`
	Slim bool                       `json:"slim,omitempty"`
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
	under := func(a *model.Aria) string {
		p := ix.get(a.Parent)
		if p == nil {
			return short(a.Parent)
		}
		if n := holds[p.ID]; len(n) > 0 {
			return strings.Join(n, "/")
		}
		return short(p.ID) + " " + p.Handle
	}
	// Newest first: a fork's ttl runs from its creation, so expiry - ttl is
	// when it was minted, fixed for its life.
	sort.Slice(forks, func(i, j int) bool {
		ci, cj := forks[i].ExpiresAt-forks[i].TTLMS, forks[j].ExpiresAt-forks[j].TTLMS
		if ci != cj {
			return ci > cj
		}
		return forks[i].ID < forks[j].ID
	})
	for i, a := range forks {
		if i == ForksMax {
			v.Forks = append(v.Forks, fmt.Sprintf("+%d more (figaro ls)", len(forks)-ForksMax))
			break
		}
		v.Forks = append(v.Forks, fmt.Sprintf("%s %s under %s", short(a.ID), a.Handle, under(a)))
	}
	return v
}

// Keys is the view as form keys.
func (v View) Keys() map[string]json.RawMessage {
	out := map[string]json.RawMessage{"forks": model.MustJSON(v.Forks)}
	if v.Forks == nil {
		out["forks"] = model.MustJSON([]string{})
	}
	for k, line := range v.Roles {
		out[k] = model.MustJSON(line)
	}
	return out
}

// Step compares a view with what was written: set holds only changed keys,
// unset the keys whose role is gone (and, once, the Dropped keys).
func Step(prev State, v View) (set map[string]json.RawMessage, unset []string, next State) {
	keys := v.Keys()
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
	return set, unset, State{Keys: keys, Slim: true}
}
