package keel

import (
	"testing"
	"time"
)

// Literal pins: a feed round long-polls 30 s, so the shortest feed interval
// is 35 s; without the feed there is no floor.
func TestMinEveryLiterals(t *testing.T) {
	if got := MinEvery(true); got != 35*time.Second {
		t.Fatalf("MinEvery(feed) = %s, want 35s", got)
	}
	if got := MinEvery(false); got != 0 {
		t.Fatalf("MinEvery(no feed) = %s, want 0", got)
	}
}

// Socket: EBAC_KEEL_SOCKET wins; unset falls back to the literal default door.
func TestSocketFromEnvElseDefault(t *testing.T) {
	t.Setenv("EBAC_KEEL_SOCKET", "/tmp/fake-door.sock")
	if got := Socket(); got != "/tmp/fake-door.sock" {
		t.Fatalf("Socket() with env = %q", got)
	}
	t.Setenv("EBAC_KEEL_SOCKET", "")
	if got := Socket(); got != "/run/kweb-ebac/threads.sock" {
		t.Fatalf("Socket() without env = %q", got)
	}
}
