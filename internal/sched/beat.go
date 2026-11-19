package sched

import (
	"time"

	"github.com/gluck/ebac/internal/budget"
)

// BeatKind marks ebacd's heartbeat form.
const BeatKind = "ebac.daemon"

// BeatEvery is how often ebacd writes its heartbeat form: a reader that sees
// beat.at older than 2 x BeatEvery knows the scheduler is dead.
const BeatEvery = time.Minute

// Beat is the heartbeat form's one key, beat.
type Beat struct {
	At      time.Time                          `json:"at"`
	EveryS  int                                `json:"every_s"`
	Critics map[string]Entry                   `json:"critics"`
	Buckets map[string]map[string]budget.Level `json:"buckets"`
}
