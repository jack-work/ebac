// Package keel is the keel plugin's configuration as the core CLI sees it:
// the scope keys a holder sets on a keel critic's role form, their
// validation, and the plugin's timing constants and socket paths. Pure: no
// I/O. Owned by the keel lane; the plugin (plugins/keel) applies the scope.
package keel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gluck/ebac/sdk/model"
)

// Mirrorable fields: kind, by and at of an inbox item are always kept.
var (
	ThreadFields = []string{"path", "line", "resolved", "n", "last_by"}
	InboxFields  = []string{"time", "thread", "path", "line", "quote", "body", "cut", "fetch"}
)

type Scope struct {
	Changed bool // threads: only threads with news this round
	Heads   bool // inbox: only a thread's first comment
	Last    int  // inbox: at most the newest N of the round's items per thread (0: all)
	// mirror maps a mirrored key to its fields (nil: every field); a nil
	// mirror mirrors every key.
	Mirror map[string]map[string]bool
}

func (s Scope) Partial() bool { return s.Changed || s.Heads || s.Last > 0 || s.Mirror != nil }

func (s Scope) Mirrors(key string) bool {
	_, ok := s.Mirror[key]
	return s.Mirror == nil || ok
}

// fields are the kept fields of a mirrored key, nil for every field.
func (s Scope) Fields(key string) map[string]bool { return s.Mirror[key] }

// CheckScope validates a scope before it is stored (ebac add-keel, ebac set).
func CheckScope(spec model.KeelScope) error {
	_, err := Compile(spec)
	return err
}

func Compile(spec model.KeelScope) (Scope, error) {
	var s Scope
	switch spec.Threads {
	case "", "all":
	case "changed":
		s.Changed = true
	default:
		return s, fmt.Errorf("%s %q: want all or changed", model.KeelThreadsScopeKey, spec.Threads)
	}
	switch in := spec.Inbox; {
	case in == "" || in == "all":
	case in == "heads":
		s.Heads = true
	case strings.HasPrefix(in, "last:"):
		n, err := strconv.Atoi(strings.TrimPrefix(in, "last:"))
		if err != nil || n < 1 {
			return s, fmt.Errorf("%s %q: want last:N with N >= 1", model.KeelInboxScopeKey, in)
		}
		s.Last = n
	default:
		return s, fmt.Errorf("%s %q: want all, heads or last:N", model.KeelInboxScopeKey, in)
	}
	if len(spec.Mirror) == 0 {
		return s, nil
	}
	s.Mirror = map[string]map[string]bool{}
	whole := map[string]bool{}
	for _, tok := range spec.Mirror {
		key, field, dotted := strings.Cut(tok, ".")
		var known []string
		switch key {
		case "threads":
			known = ThreadFields
		case "inbox":
			known = InboxFields
		default:
			return s, fmt.Errorf("%s %q: want threads or inbox, optionally .FIELD", model.KeelMirrorKey, tok)
		}
		if !dotted {
			whole[key] = true
			s.Mirror[key] = nil
			continue
		}
		if !contains(known, field) {
			return s, fmt.Errorf("%s %q: %s fields are %s", model.KeelMirrorKey, tok, key, strings.Join(known, ", "))
		}
		if whole[key] {
			continue
		}
		if s.Mirror[key] == nil {
			s.Mirror[key] = map[string]bool{}
		}
		s.Mirror[key][field] = true
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
