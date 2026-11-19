package poll

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// RED if the lock goes: a second taker of a held round lock is told held; after unlock it is free again.
func TestRoundLockIsExclusiveAndReleased(t *testing.T) {
	dir := t.TempDir()
	unlock, held, err := lockRound(dir, "author-jack")
	if err != nil || held {
		t.Fatalf("first take: held=%v err=%v", held, err)
	}
	if _, held2, err := lockRound(dir, "author-jack"); err != nil || !held2 {
		t.Fatalf("second take while held: held=%v err=%v, want held", held2, err)
	}
	if _, heldOther, _ := lockRound(dir, "author-chiqun"); heldOther {
		t.Fatal("another critic's lock was held")
	}
	unlock()
	u2, held3, err := lockRound(dir, "author-jack")
	if err != nil || held3 {
		t.Fatalf("after unlock: held=%v err=%v", held3, err)
	}
	u2()
	if _, _, err := lockRound(dir, "../x"); err == nil {
		t.Fatal("a path-like name was locked")
	}
}

// Poll with the critic's round held elsewhere skips with one exact line and runs nothing.
func TestPollSkipsWhenTheRoundIsHeld(t *testing.T) {
	dir := t.TempDir()
	unlock, _, err := lockRound(dir, "author-jack")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out bytes.Buffer
	p := &Poller{Out: &out, BackoffDir: dir, PauseFile: dir + "/no-pause"} // Store nil: a round that ran would panic
	if err := p.Poll(context.Background(), "author-jack"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "author-jack: a round is already running (ebacd or a hand poll): skipped" {
		t.Fatalf("out %q", got)
	}
}
