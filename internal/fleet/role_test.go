package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/figaro"
)

// forms is a fake store: id -> keys; an id mapped to nil is gone; "@err" is unreadable.
func forms(m map[string]map[string]any) FormReader {
	return func(_ context.Context, id string) (map[string]any, error) {
		if id == "@err" {
			return nil, errors.New("connection closed")
		}
		f, ok := m[id]
		if !ok || f == nil {
			return nil, fmt.Errorf("read %s: %w", id, figaro.ErrFormGone)
		}
		return f, nil
	}
}

// Every arm: standing, absorbed (followed one and two hops), retired (gone, or the absorber gone), and the errors
// (unreadable, cycle, a chain past MaxRoleHops). RED if absorption reads as retirement, an unreadable role as retired,
// or the chain is unbounded.
func TestFollowRole(t *testing.T) {
	store := forms(map[string]map[string]any{
		"@a": {"target-aria": "x"},
		"@b": {"moved-to": "@a"},
		"@c": {"moved-to": "@b"},
		"@d": {"moved-to": "@gone"},
		"@e": {"moved-to": "@err"},
		"@y": {"moved-to": "@z"}, "@z": {"moved-to": "@y"},
		"@1": {"moved-to": "@2"}, "@2": {"moved-to": "@3"}, "@3": {"moved-to": "@4"}, "@4": {"moved-to": "@5"},
		"@5": {"moved-to": "@6"}, "@6": {"moved-to": "@7"}, "@7": {"target-aria": "x"},
	})
	for _, c := range []struct {
		id, cur string
		retired bool
		why     string
		err     string
	}{
		{"@a", "@a", false, "", ""},
		{"@b", "@a", false, "", ""},
		{"@c", "@a", false, "", ""},
		{"@gone", "@gone", true, "retired: its form is gone", ""},
		{"@d", "@gone", true, "retired: absorbed into @gone, whose form is gone", ""},
		{"@err", "", false, "", "unreadable"},
		{"@e", "", false, "", "unreadable"},
		{"@y", "", false, "", "cycle"},
		{"@1", "", false, "", "longer than 5 hops"},
	} {
		cur, retired, why, err := FollowRole(context.Background(), store, c.id)
		if cur != c.cur || retired != c.retired || why != c.why || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: cur %q retired %v why %q err %v", c.id, cur, retired, why, err)
		}
	}
}

// A with-role end is allowed only on a standing roster role holding a target-aria. RED if a plain form, a retired or
// an absorbed role is accepted.
func TestIsRosterRole(t *testing.T) {
	store := forms(map[string]map[string]any{"@a": {"target-aria": "x"}, "@plain": {"name": "ebac-x"}, "@b": {"moved-to": "@a", "target-aria": "x"}})
	if err := IsRosterRole(context.Background(), store, "@a"); err != nil {
		t.Fatalf("a held role refused: %v", err)
	}
	for id, want := range map[string]string{"@plain": "not a roster role", "@gone": "not in the roster", "@b": "absorbed into @a", "@err": "unreadable"} {
		if err := IsRosterRole(context.Background(), store, id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error naming %q", id, err, want)
		}
	}
}
