package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/gluck/ebac/internal/figaro"
)

// FormReader reads a form's keys (figaro.Figaro.Form; a fake in tests).
type FormReader func(ctx context.Context, id string) (map[string]any, error)

// RoleStanding says whether a role is still in the roster, for a with-role end:
// present, or gone with why (retired: its form is gone; absorbed: moved-to set).
// An unreadable role is an error, never "gone": a figaro outage must not end
// every standing critic.
func RoleStanding(ctx context.Context, read FormReader, id string) (present bool, why string, err error) {
	f, err := read(ctx, id)
	if errors.Is(err, figaro.ErrFormGone) {
		return false, "retired: its form is gone", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("role %s unreadable: %w", id, err)
	}
	if to, ok := f["moved-to"]; ok && to != nil {
		return false, fmt.Sprintf("absorbed into %v", to), nil
	}
	return true, "", nil
}

// IsRosterRole says whether id is a role a with-role end may follow now: in the
// roster (RoleStanding present) and a role (it has a target-aria).
func IsRosterRole(ctx context.Context, read FormReader, id string) error {
	present, why, err := RoleStanding(ctx, read, id)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s is not in the roster (%s)", id, why)
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
