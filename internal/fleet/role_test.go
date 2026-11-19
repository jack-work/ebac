package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/figaro"
)

func reader(f map[string]any, err error) FormReader {
	return func(context.Context, string) (map[string]any, error) { return f, err }
}

// Every arm of a role's standing: present (held or between holders), retired (form gone), absorbed (moved-to), and
// unreadable (an error, never "gone": a figaro outage must not end standing critics). RED if an unreadable role reads
// as gone, or moved-to is ignored.
func TestRoleStanding(t *testing.T) {
	gone := fmt.Errorf("read @r: %w", figaro.ErrFormGone)
	for _, c := range []struct {
		name    string
		read    FormReader
		present bool
		why     string
		err     bool
	}{
		{"held", reader(map[string]any{"target-aria": "abc"}, nil), true, "", false},
		{"between holders", reader(map[string]any{"name": "x"}, nil), true, "", false},
		{"retired", reader(nil, gone), false, "retired: its form is gone", false},
		{"absorbed", reader(map[string]any{"moved-to": "@f9469b49"}, nil), false, "absorbed into @f9469b49", false},
		{"unreadable", reader(nil, errors.New("connection closed")), false, "", true},
	} {
		present, why, err := RoleStanding(context.Background(), c.read, "@r")
		if present != c.present || why != c.why || (err != nil) != c.err {
			t.Errorf("%s: present=%v why=%q err=%v", c.name, present, why, err)
		}
	}
}

// A with-role end is allowed only on a roster role: in the roster AND holding a target-aria. RED if a plain form or a
// retired role is accepted.
func TestIsRosterRole(t *testing.T) {
	if err := IsRosterRole(context.Background(), reader(map[string]any{"target-aria": "abc"}, nil), "@r"); err != nil {
		t.Fatalf("a held role refused: %v", err)
	}
	for _, c := range []struct {
		name string
		read FormReader
		want string
	}{
		{"a plain form", reader(map[string]any{"name": "ebac-x"}, nil), "not a roster role"},
		{"a retired role", reader(nil, fmt.Errorf("x: %w", figaro.ErrFormGone)), "not in the roster"},
		{"an absorbed role", reader(map[string]any{"moved-to": "@n", "target-aria": "a"}, nil), "not in the roster"},
		{"unreadable", reader(nil, errors.New("connection closed")), "unreadable"},
	} {
		if err := IsRosterRole(context.Background(), c.read, "@r"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
	}
}
