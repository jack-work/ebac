package keel

import (
	"os"
	"time"
)

// Timing of a keel critic's rounds.
const (
	FeedWait     = 30 * time.Second
	FeedEvery    = 45 * time.Second
	ThreadsEvery = 5 * time.Minute
)

// MinEvery is the shortest poll interval: with the feed, a round's long-poll
// must fit inside it.
func MinEvery(feed bool) time.Duration {
	if feed {
		return FeedWait + 5*time.Second
	}
	return 0
}

// DefaultSocket is ebac's read door to keel: admits uid 988 named ebac only,
// and only GET /api/r/<viewer-id>/threads, the events feed and, for listed
// repos, refs, span and log.
const DefaultSocket = "/run/kweb-ebac/threads.sock"

// Socket is EBAC_KEEL_SOCKET, else DefaultSocket.
func Socket() string {
	if s := os.Getenv("EBAC_KEEL_SOCKET"); s != "" {
		return s
	}
	return DefaultSocket
}

// Review keys on a keel critic's role form.
const (
	ReviewKey      = "review"
	VerdictKey     = "verdict"
	ReviewStateKey = "review_state"
)
