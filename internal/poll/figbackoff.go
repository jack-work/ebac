package poll

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Figaro unavailability (figaro#115). A poll round that cannot reach the
// figaro daemon backs off per critic, exponentially with jitter: 5 s, 10 s,
// 20 s ... capped at 2 min, never an immediate retry. The state is a file
// beside the store, not the form, because the form is exactly what is
// unreachable. A round that reaches the daemon clears it.
//
// A bounce of the daemon announces itself first: while PauseFile exists,
// every round is skipped with one line and nothing touches the socket.
const (
	PauseFile        = "/run/figaro-bounce/pause"
	figBackoffStart  = 5 * time.Second
	figBackoffCap    = 2 * time.Minute
	figBackoffSubdir = "figaro-backoff"
)

// Paused says whether a figaro bounce has asked ebac to stand still.
func Paused(file string) bool {
	if file == "" {
		file = PauseFile
	}
	_, err := os.Stat(file)
	return err == nil
}

type figBackoff struct {
	Attempt int       `json:"attempt"`
	Until   time.Time `json:"until"`
	Last    string    `json:"last,omitempty"`
}

// figBackoffDelay is attempt n's wait: start*2^(n-1) capped, minus up to a
// fifth at random, so critics that failed together do not retry together and
// no wait exceeds the cap.
func figBackoffDelay(attempt int, rnd func(int64) int64) time.Duration {
	d := figBackoffCap
	if attempt < 6 {
		d = min(figBackoffStart<<(attempt-1), figBackoffCap)
	}
	return d - time.Duration(rnd(int64(d)/5+1))
}

func figBackoffPath(dir, critic string) string {
	return filepath.Join(dir, figBackoffSubdir, critic+".json")
}

func readFigBackoff(dir, critic string) figBackoff {
	var b figBackoff
	if raw, err := os.ReadFile(figBackoffPath(dir, critic)); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// noteFigFailure records one more unreachable round and returns the backoff.
func noteFigFailure(dir, critic string, now time.Time, cause error, rnd func(int64) int64) (figBackoff, error) {
	b := readFigBackoff(dir, critic)
	b.Attempt++
	b.Until = now.Add(figBackoffDelay(b.Attempt, rnd))
	b.Last = cause.Error()
	if len(b.Last) > 300 {
		b.Last = b.Last[:300]
	}
	p := figBackoffPath(dir, critic)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return b, err
	}
	raw, _ := json.Marshal(b)
	return b, os.WriteFile(p, raw, 0o644)
}

func clearFigBackoff(dir, critic string) {
	_ = os.Remove(figBackoffPath(dir, critic))
}

var figRand = rand.Int63n

func fmtBackoff(critic string, b figBackoff) string {
	return fmt.Sprintf("%s: figaro unreachable (attempt %d), next round not before %s: %s",
		critic, b.Attempt, b.Until.UTC().Format(time.RFC3339), b.Last)
}
