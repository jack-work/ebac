package keel

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// A scope trades completeness for size: rows (which threads, which inbox
// items) and columns (which keys and fields) of a round reach the form. It is
// applied after the diff, so wake tiers, events and the seen state are those
// of the full fetch whatever the scope drops.

// Mirrorable fields: kind, by and at of an inbox item are always kept.
var (
	threadFields = []string{"path", "line", "resolved", "n", "last_by"}
	inboxFields  = []string{"time", "thread", "path", "line", "quote", "body", "cut", "fetch"}
)

type scope struct {
	changed bool // threads: only threads with news this round
	heads   bool // inbox: only a thread's first comment
	last    int  // inbox: at most the newest N of the round's items per thread (0: all)
	// mirror maps a mirrored key to its fields (nil: every field); a nil
	// mirror mirrors every key.
	mirror map[string]map[string]bool
}

func (s scope) partial() bool { return s.changed || s.heads || s.last > 0 || s.mirror != nil }

func (s scope) mirrors(key string) bool {
	_, ok := s.mirror[key]
	return s.mirror == nil || ok
}

// fields are the kept fields of a mirrored key, nil for every field.
func (s scope) fields(key string) map[string]bool { return s.mirror[key] }

// CheckScope validates a scope before it is stored (ebac add-keel, ebac set).
func CheckScope(spec model.KeelScope) error {
	_, err := compileScope(spec)
	return err
}

func compileScope(spec model.KeelScope) (scope, error) {
	var s scope
	switch spec.Threads {
	case "", "all":
	case "changed":
		s.changed = true
	default:
		return s, fmt.Errorf("%s %q: want all or changed", model.KeelThreadsScopeKey, spec.Threads)
	}
	switch in := spec.Inbox; {
	case in == "" || in == "all":
	case in == "heads":
		s.heads = true
	case strings.HasPrefix(in, "last:"):
		n, err := strconv.Atoi(strings.TrimPrefix(in, "last:"))
		if err != nil || n < 1 {
			return s, fmt.Errorf("%s %q: want last:N with N >= 1", model.KeelInboxScopeKey, in)
		}
		s.last = n
	default:
		return s, fmt.Errorf("%s %q: want all, heads or last:N", model.KeelInboxScopeKey, in)
	}
	if len(spec.Mirror) == 0 {
		return s, nil
	}
	s.mirror = map[string]map[string]bool{}
	whole := map[string]bool{}
	for _, tok := range spec.Mirror {
		key, field, dotted := strings.Cut(tok, ".")
		var known []string
		switch key {
		case "threads":
			known = threadFields
		case "inbox":
			known = inboxFields
		default:
			return s, fmt.Errorf("%s %q: want threads or inbox, optionally .FIELD", model.KeelMirrorKey, tok)
		}
		if !dotted {
			whole[key] = true
			s.mirror[key] = nil
			continue
		}
		if !contains(known, field) {
			return s, fmt.Errorf("%s %q: %s fields are %s", model.KeelMirrorKey, tok, key, strings.Join(known, ", "))
		}
		if whole[key] {
			continue
		}
		if s.mirror[key] == nil {
			s.mirror[key] = map[string]bool{}
		}
		s.mirror[key][field] = true
	}
	return s, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ParseMirror reads a comma-separated mirror list (flags).
func ParseMirror(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ScopeKeys are the role-form keys a scope is stored in: only the set ones.
func ScopeKeys(spec model.KeelScope) map[string]json.RawMessage {
	keys := map[string]json.RawMessage{}
	if spec.Threads != "" {
		keys[model.KeelThreadsScopeKey] = model.MustJSON(spec.Threads)
	}
	if spec.Inbox != "" {
		keys[model.KeelInboxScopeKey] = model.MustJSON(spec.Inbox)
	}
	if len(spec.Mirror) > 0 {
		keys[model.KeelMirrorKey] = model.MustJSON(spec.Mirror)
	}
	return keys
}

// ScopeFromKeys reads the keys: threads_scope and inbox_scope are strings,
// mirror a JSON list or one comma-separated string. Absent or empty is unset.
func ScopeFromKeys(keys map[string]json.RawMessage) (model.KeelScope, error) {
	var spec model.KeelScope
	str := func(key string, into *string) error {
		raw := keys[key]
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return fmt.Errorf("%s: want a string, got %s", key, raw)
		}
		*into = strings.TrimSpace(*into)
		return nil
	}
	if err := str(model.KeelThreadsScopeKey, &spec.Threads); err != nil {
		return spec, err
	}
	if err := str(model.KeelInboxScopeKey, &spec.Inbox); err != nil {
		return spec, err
	}
	if raw := keys[model.KeelMirrorKey]; len(raw) > 0 && string(raw) != "null" {
		var list []string
		var one string
		switch {
		case json.Unmarshal(raw, &list) == nil:
		case json.Unmarshal(raw, &one) == nil:
			list = strings.Split(one, ",")
		default:
			return spec, fmt.Errorf("%s: want a list or a comma-separated string, got %s", model.KeelMirrorKey, raw)
		}
		for _, v := range list {
			if v = strings.TrimSpace(v); v != "" {
				spec.Mirror = append(spec.Mirror, v)
			}
		}
	}
	return spec, nil
}

// resolveScope is the scope in force and the error to show on the form ("" when
// the keys are absent or valid). A valid scope becomes the last good one; a
// broken one leaves the last good one in force (else everything is written).
func resolveScope(st *model.KeelState, keys map[string]json.RawMessage) (scope, string) {
	spec, err := ScopeFromKeys(keys)
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
	if s.changed && r.index != nil {
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
	if s.heads || s.last > 0 {
		byThread := map[string][]string{}
		for k, it := range r.items {
			if s.heads && r.pos[k] != 0 {
				delete(r.items, k)
				continue
			}
			byThread[it.Thread] = append(byThread[it.Thread], k)
		}
		if s.last > 0 {
			for _, ks := range byThread {
				sort.Slice(ks, func(i, j int) bool { return r.pos[ks[i]] > r.pos[ks[j]] })
				for _, k := range ks[min(s.last, len(ks)):] {
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

func cutItem(it form.InboxItem, keep map[string]bool) form.InboxItem {
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
	if s.changed {
		parts = append(parts, "key threads lists only the threads with news in the latest round that had any (a seat that slept through a round does not see that round's threads there)")
	}
	if s.heads {
		parts = append(parts, "the inbox holds only the first comment of a thread")
	}
	if s.last > 0 {
		parts = append(parts, fmt.Sprintf("the inbox holds at most the %d newest comments of a thread per round", s.last))
	}
	if s.mirror != nil {
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
