package keel

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	keelcfg "github.com/gluck/ebac/sdk/cfg/keel"
	"github.com/gluck/ebac/sdk/model"
)

// A scope trades completeness for size: rows (which threads, which inbox
// items) and columns (which keys and fields) of a round reach the form. It is
// applied after the diff, so wake tiers, events and the seen state are those
// of the full fetch whatever the scope drops.

// scope is the configured scope (keelcfg.Scope) with the plugin's apply.
type scope struct{ keelcfg.Scope }

func compileScope(spec model.KeelScope) (scope, error) {
	c, err := keelcfg.Compile(spec)
	return scope{c}, err
}

func (s scope) partial() bool                     { return s.Partial() }
func (s scope) mirrors(key string) bool           { return s.Mirrors(key) }
func (s scope) fields(key string) map[string]bool { return s.Fields(key) }

// resolveScope is the scope in force and the error to show on the form ("" when
// the keys are absent or valid). A valid scope becomes the last good one; a
// broken one leaves the last good one in force (else everything is written).
func resolveScope(st *model.KeelState, keys map[string]json.RawMessage) (scope, string) {
	spec, err := keelcfg.ScopeFromKeys(keys)
	var s scope
	if err == nil {
		s, err = compileScope(spec)
	}
	if err == nil {
		st.Scope = nil
		if !spec.IsZero() {
			st.Scope = &spec
		}
		return s, ""
	}
	in := "no scope (everything written)"
	s = scope{}
	if st.Scope != nil {
		if g, gerr := compileScope(*st.Scope); gerr == nil {
			s, in = g, "the last good scope"
		}
	}
	return s, fmt.Sprintf("scope rejected, %s is in force: %v", in, err)
}

// apply cuts a round to the scope: rows first, then columns. It never touches
// r.events.
func (s scope) apply(r *keelRound) {
	if s.Changed && r.index != nil {
		news := map[string]bool{}
		for _, e := range r.events {
			news[e.ThreadID] = true
		}
		index := map[string]IndexEntry{}
		for id, ix := range r.index {
			if news[id] {
				index[id] = ix
			}
		}
		r.index = index
		if len(index) == 0 {
			r.index = nil // a quiet round leaves the last news in place
		}
	}
	if s.Heads || s.Last > 0 {
		byThread := map[string][]string{}
		for k, it := range r.items {
			if s.Heads && r.pos[k] != 0 {
				delete(r.items, k)
				continue
			}
			byThread[it.Thread] = append(byThread[it.Thread], k)
		}
		if s.Last > 0 {
			for _, ks := range byThread {
				sort.Slice(ks, func(i, j int) bool { return r.pos[ks[i]] > r.pos[ks[j]] })
				for _, k := range ks[min(s.Last, len(ks)):] {
					delete(r.items, k)
				}
			}
		}
	}
	if !s.mirrors("inbox") {
		r.items = nil
	} else if f := s.fields("inbox"); f != nil {
		for k, it := range r.items {
			r.items[k] = cutItem(it, f)
		}
	}
}

func cutItem(it model.InboxItem, keep map[string]bool) model.InboxItem {
	if !keep["time"] {
		it.Time = ""
	}
	if !keep["thread"] {
		it.Thread = ""
	}
	if !keep["path"] {
		it.Path = ""
	}
	if !keep["line"] {
		it.Line = 0
	}
	if !keep["quote"] {
		it.Quote = ""
	}
	if !keep["body"] {
		it.Body = ""
	}
	if !keep["cut"] {
		it.Cut = false
	}
	if !keep["fetch"] {
		it.Fetch = ""
	}
	return it
}

// indexValue is the threads key's value: the index itself, or, under a field
// mirror, each entry cut to its kept fields.
func (s scope) indexValue(index map[string]IndexEntry) any {
	keep := s.fields("threads")
	if keep == nil {
		return index
	}
	out := map[string]map[string]json.RawMessage{}
	for id, ix := range index {
		var all map[string]json.RawMessage
		_ = json.Unmarshal(model.MustJSON(ix), &all)
		row := map[string]json.RawMessage{}
		for f, v := range all {
			if keep[f] {
				row[f] = v
			}
		}
		out[id] = row
	}
	return out
}

// describe is the brief's line for a partial scope: what the form leaves out
// and where the full view is.
func (s scope) describe(repo string) string {
	var parts []string
	if s.Changed {
		parts = append(parts, "key threads lists only the threads with news in the latest round that had any (a seat that slept through a round does not see that round's threads there)")
	}
	if s.Heads {
		parts = append(parts, "the inbox holds only the first comment of a thread")
	}
	if s.Last > 0 {
		parts = append(parts, fmt.Sprintf("the inbox holds at most the %d newest comments of a thread per round", s.Last))
	}
	if s.Mirror != nil {
		var m []string
		for _, k := range []string{"threads", "inbox"} {
			if !s.mirrors(k) {
				continue
			}
			if f := s.fields(k); f != nil {
				var fs []string
				for x := range f {
					fs = append(fs, x)
				}
				sort.Strings(fs)
				k += " (" + strings.Join(fs, ", ") + ")"
			}
			m = append(m, k)
		}
		if len(m) == 0 {
			m = []string{"neither threads nor inbox"}
		}
		parts = append(parts, "the form mirrors "+strings.Join(m, " and "))
	}
	return "Partial scope: " + strings.Join(parts, "; ") + ". The full view: keel " + repo + ": GET " + ThreadsPath(repo) + "."
}
