// Package livetest holds what every EBAC_LIVE test shares: the end of what it made on the live daemon.
package livetest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Remover removes a form (figaro.Figaro).
type Remover interface {
	FormRemove(ctx context.Context, id string) error
}

// Remove ends the forms a live test made when the test ends, passed or FAILED (t.Cleanup runs either way): a
// critic's role form gone, nothing lists, polls or re-arms it (11-19: livetest-* critics left by failed runs were
// re-armed by reconcile and polled Teams). Each removal has its own deadline, never the test's context (a cancelled
// one made the removal fail unseen), and a failed removal fails the test instead of being swallowed. A form already
// gone is fine.
func Remove(t testing.TB, f Remover, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := f.FormRemove(ctx, id)
			cancel()
			if err != nil && !gone(err) {
				t.Errorf("livetest: form %s left behind: %v", id, err)
			}
		}
	})
}

// gone is the daemon saying the form no longer exists (as figaro.Gone; this package cannot import figaro, whose own
// tests use it).
func gone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "not found") || strings.Contains(s, "not in tree") || strings.Contains(s, "unknown trunk")
}
