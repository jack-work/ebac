package main

import (
	"testing"
	"time"

	"github.com/gluck/ebac/internal/source/keel"
)

func TestAFeedCriticPollsWithinAMinuteByDefault(t *testing.T) {
	got, err := keelEvery(0, true)
	if err != nil || got != 45*time.Second {
		t.Fatalf("feed default %s %v", got, err)
	}
	if got <= keel.FeedWait || got > time.Minute {
		t.Fatalf("feed default %s must start after a held round (%s) and stay near real time", got, keel.FeedWait)
	}
	if got, err := keelEvery(0, false); err != nil || got != 5*time.Minute {
		t.Fatalf("threads default %s %v: diffing /threads stays at 5m", got, err)
	}
}

func TestEveryOverridesTheDefault(t *testing.T) {
	for _, c := range []struct {
		every time.Duration
		feed  bool
	}{{2 * time.Minute, true}, {35 * time.Second, true}, {30 * time.Second, false}} {
		if got, err := keelEvery(c.every, c.feed); err != nil || got != c.every {
			t.Errorf("--every %s feed=%v: %s %v", c.every, c.feed, got, err)
		}
	}
}

func TestAFeedRoundMayNotOverlapTheNext(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 30 * time.Second, 34 * time.Second, -time.Second} {
		if _, err := keelEvery(d, true); err == nil {
			t.Errorf("--every %s accepted with a %s long-poll", d, keel.FeedWait)
		}
	}
}
