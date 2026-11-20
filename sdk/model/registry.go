package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// Registration is one critic declared in ebac's configuration (the system
// flake's services.ebac renders /etc/ebac/config.json key "registrations";
// rework step 3): its role form and its critic key as declared.
type Registration struct {
	Form   string          `json:"form"`
	Critic json.RawMessage `json:"critic"`
}

// LoadRegistrations reads the "registrations" key of path. declared is false
// when the file or the key is absent (nothing declared yet: no critic is
// flagged undeclared); an unreadable file is an error.
func LoadRegistrations(path string) (regs map[string]Registration, declared bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var f struct {
		Registrations map[string]Registration `json:"registrations"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return f.Registrations, f.Registrations != nil, nil
}

// UndeclaredNote is the ebac ls health of a live critic the configuration
// does not declare (step 3 flags it; a later switch archives it).
const UndeclaredNote = "undeclared: not in ebac's configuration (services.ebac)"

// CheckRegistrations is `ebac config check`: one line per difference between
// the declared critics and the live ones (live: name -> role form id and its
// critic key as stored). No lines = the configuration generates exactly the
// live set.
func CheckRegistrations(regs map[string]Registration, live map[string]Registration) []string {
	var out []string
	for name, r := range regs {
		l, ok := live[name]
		if !ok {
			out = append(out, fmt.Sprintf("%s: declared (form %s) but no live critic", name, r.Form))
			continue
		}
		if l.Form != r.Form {
			out = append(out, fmt.Sprintf("%s: form %s declared, %s live", name, r.Form, l.Form))
		}
		for _, k := range diffKeys(r.Critic, l.Critic) {
			out = append(out, fmt.Sprintf("%s: critic.%s differs", name, k))
		}
	}
	for name, l := range live {
		if _, ok := regs[name]; !ok {
			out = append(out, fmt.Sprintf("%s: live (form %s) but undeclared", name, l.Form))
		}
	}
	sort.Strings(out)
	return out
}

// diffKeys names the top-level keys whose JSON values differ (a key absent on
// one side and empty/zero on the other is equal).
func diffKeys(a, b json.RawMessage) []string {
	var ma, mb map[string]json.RawMessage
	_ = json.Unmarshal(a, &ma)
	_ = json.Unmarshal(b, &mb)
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if !jsonEqual(ma[k], mb[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func jsonEqual(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) string {
		if len(bytes.TrimSpace(r)) == 0 {
			return "null"
		}
		var v any
		if json.Unmarshal(r, &v) != nil {
			return string(r)
		}
		switch x := v.(type) {
		case nil:
			return "null"
		case bool:
			if !x {
				return "null"
			}
		case string:
			if x == "" {
				return "null"
			}
		case float64:
			if x == 0 {
				return "null"
			}
		case []any:
			if len(x) == 0 {
				return "null"
			}
		case map[string]any:
			if len(x) == 0 {
				return "null"
			}
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return norm(a) == norm(b)
}
