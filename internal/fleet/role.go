package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/gluck/ebac/internal/figaro"
)

// FormReader reads a form's keys (figaro.Figaro.Form; a fake in tests).
type FormReader func(ctx context.Context, id string) (map[string]any, error)

// MaxRoleHops bounds a moved-to chain: a longer one (or a cycle) is an error.
const MaxRoleHops = 5

// FollowRole is a role's standing for a with-role end. An ABSORBED role (its
// form has moved-to) is followed along the chain, at most MaxRoleHops, to the
// role that stands now (current; the caller repoints to it, the critic does not
// end). A RETIRED role (its form gone, no moved-to to follow) is retired, with
// why. An unreadable role, a cycle or an overlong chain is an error, never
// "retired": a figaro outage must not end standing critics.
func FollowRole(ctx context.Context, read FormReader, id string) (current string, retired bool, why string, err error) {
	seen := map[string]bool{}
	cur := id
	for hop := 0; ; hop++ {
		if seen[cur] {
			return "", false, "", fmt.Errorf("role %s: moved-to cycle at %s", id, cur)
		}
		if hop > MaxRoleHops {
			return "", false, "", fmt.Errorf("role %s: moved-to chain longer than %d hops", id, MaxRoleHops)
		}
		seen[cur] = true
		f, err := read(ctx, cur)
		if errors.Is(err, figaro.ErrFormGone) {
			if cur != id {
				return cur, true, fmt.Sprintf("retired: absorbed into %s, whose form is gone", cur), nil
			}
			return cur, true, "retired: its form is gone", nil
		}
		if err != nil {
			return "", false, "", fmt.Errorf("role %s unreadable: %w", cur, err)
		}
		to, _ := f["moved-to"].(string)
		if to == "" {
			return cur, false, "", nil
		}
		cur = to
	}
}

// IsRosterRole says whether id is a role a with-role end may follow now: it
// stands (not retired, not absorbed: name the absorbing role instead) and is a
// role (it has a target-aria).
func IsRosterRole(ctx context.Context, read FormReader, id string) error {
	cur, retired, why, err := FollowRole(ctx, read, id)
	switch {
	case err != nil:
		return err
	case retired:
		return fmt.Errorf("%s is not in the roster (%s)", id, why)
	case cur != id:
		return fmt.Errorf("%s was absorbed into %s: name that role", id, cur)
	}
	f, err := read(ctx, id)
	if err != nil {
		return fmt.Errorf("role %s unreadable: %w", id, err)
	}
	if f["target-aria"] == nil {
		return fmt.Errorf("%s is a form, not a roster role (no target-aria)", id)
	}
	return nil
}
