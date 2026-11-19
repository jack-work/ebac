// Package budget meters what ebac spends on a shared credential (GitHub's
// GraphQL points and REST calls are per ACCOUNT, not per critic). One token
// bucket per (credential, limit); a round reserves its estimate before it
// runs and settles what it actually spent afterwards. When the account-wide
// remaining count reported by the upstream falls below a floor, every
// reservation on that credential waits for the reset, inside its share or
// not. Priority classes decide who waits when a bucket is short.
//
// The ledger persists to one JSON file, so a restart resumes from the
// levels it left rather than from full buckets.
package budget

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Class is a registration's priority when a bucket is short.
type Class string

const (
	// WakeCritical may drain a bucket to zero: a seat is waiting on it.
	WakeCritical Class = "wake-critical"
	// Scan keeps a share of the bucket for wake-critical rounds.
	Scan Class = "scan"
	// Backfill keeps the largest share back: it can always wait.
	Backfill Class = "backfill"
)

// Limit is one bucket's configuration.
type Limit struct {
	PerHour float64 `json:"per_hour"`
	Burst   float64 `json:"burst"`
	// Floor: when the upstream's account-wide remaining is below it, every
	// reservation on the credential defers until the upstream's reset.
	// 0 = no floor.
	Floor int `json:"floor,omitempty"`
}

// Config is every metered credential's limits and the share each class
// leaves in a bucket.
type Config struct {
	// Credentials maps a credential id to its limits by name ("graphql",
	// "rest"). A credential not listed is unmetered.
	Credentials map[string]map[string]Limit `json:"credentials"`
	// Keep is the fraction of a bucket's burst a class may not spend into.
	Keep map[Class]float64 `json:"keep"`
}

// DefaultKeep: wake-critical drains to 0, scan keeps 20 %, backfill 50 %.
var DefaultKeep = map[Class]float64{WakeCritical: 0, Scan: 0.2, Backfill: 0.5}

func (c Config) keep(cl Class) float64 {
	if k, ok := c.Keep[cl]; ok {
		return k
	}
	if k, ok := DefaultKeep[cl]; ok {
		return k
	}
	return DefaultKeep[Backfill] // an unknown class waits like backfill
}

// Spend is what one round spent on one limit, with the upstream's own
// account-wide view when it reported one.
type Spend struct {
	Limit     string    `json:"limit"`
	N         float64   `json:"n"`
	Remaining int       `json:"remaining"`         // -1 = not reported
	ResetAt   time.Time `json:"reset_at,omitzero"` // zero = not reported
}

// Level is one bucket's state, persisted and shown.
type Level struct {
	Tokens    float64   `json:"tokens"`
	At        time.Time `json:"at"`
	Remaining int       `json:"remaining"` // account-wide, last reported; -1 = never
	ResetAt   time.Time `json:"reset_at,omitzero"`
}

// Grant is a reservation: settle it once the round has run.
type Grant struct {
	Cred string
	Est  map[string]float64
}

// Ledger is the buckets. Safe for concurrent use.
type Ledger struct {
	cfg  Config
	path string // "" = in memory only
	now  func() time.Time

	mu     sync.Mutex
	levels map[string]map[string]*Level // cred -> limit -> level
}

// Open loads the ledger at path ("" = in memory). A missing file starts every
// bucket full; an unreadable one starts every bucket EMPTY (a torn ledger
// must not hand out a burst it may already have spent). now is the clock.
func Open(cfg Config, path string, now func() time.Time) (*Ledger, error) {
	l := &Ledger{cfg: cfg, path: path, now: now, levels: map[string]map[string]*Level{}}
	t := now()
	for cred, lims := range cfg.Credentials {
		l.levels[cred] = map[string]*Level{}
		for name, lim := range lims {
			l.levels[cred][name] = &Level{Tokens: lim.Burst, At: t, Remaining: -1}
		}
	}
	if path == "" {
		return l, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	var saved map[string]map[string]*Level
	if err == nil {
		err = json.Unmarshal(raw, &saved)
	}
	if err != nil {
		for _, lims := range l.levels {
			for _, lv := range lims {
				lv.Tokens = 0
			}
		}
		return l, fmt.Errorf("budget ledger %s unreadable, every bucket starts empty: %w", path, err)
	}
	for cred, lims := range saved {
		for name, lv := range lims {
			if cur, ok := l.levels[cred][name]; ok && lv != nil {
				*cur = *lv
				if cur.Tokens > l.cfg.Credentials[cred][name].Burst {
					cur.Tokens = l.cfg.Credentials[cred][name].Burst
				}
			}
		}
	}
	return l, nil
}

// refill brings lv up to now at lim's rate, capped at its burst.
func refill(lv *Level, lim Limit, now time.Time) {
	if dt := now.Sub(lv.At).Seconds(); dt > 0 {
		lv.Tokens = math.Min(lim.Burst, lv.Tokens+dt*lim.PerHour/3600)
		lv.At = now
	}
}

// Reserve asks for est (by limit name) on cred for a round of class cl. It
// returns a Grant, or the time to ask again. An unmetered credential, or a
// limit the credential does not meter, is always granted. An estimate above
// a bucket's burst asks only for the burst, so a large round is not starved
// forever; settling charges what it really spent.
func (l *Ledger) Reserve(cred string, cl Class, est map[string]float64) (Grant, time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	lims, metered := l.cfg.Credentials[cred]
	if !metered {
		return Grant{Cred: cred, Est: est}, time.Time{}, true
	}
	var wait time.Time
	for name, lim := range lims {
		lv := l.levels[cred][name]
		refill(lv, lim, now)
		if lim.Floor > 0 && lv.Remaining >= 0 && lv.Remaining < lim.Floor && lv.ResetAt.After(now) {
			wait = later(wait, lv.ResetAt)
			continue
		}
		need, asked := math.Min(est[name], lim.Burst), est[name] > 0
		if !asked {
			continue
		}
		keep := l.cfg.keep(cl) * lim.Burst
		if short := need + keep - lv.Tokens; short > 0 {
			if lim.PerHour <= 0 {
				wait = later(wait, now.Add(time.Hour))
				continue
			}
			wait = later(wait, now.Add(time.Duration(math.Ceil(short*3600/lim.PerHour*1e3))*time.Millisecond))
		}
	}
	if !wait.IsZero() {
		return Grant{}, wait, false
	}
	for name := range lims {
		l.levels[cred][name].Tokens -= est[name]
	}
	l.save()
	return Grant{Cred: cred, Est: est}, time.Time{}, true
}

// Settle charges a granted round what it really spent: the difference from
// its estimate goes back to (or comes out of) the bucket, which may go below
// zero (a debt the refill pays before anything else is granted). The
// upstream's account-wide remaining and reset are recorded for the floor.
func (l *Ledger) Settle(g Grant, spent []Spend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lims, metered := l.cfg.Credentials[g.Cred]
	if !metered {
		return
	}
	now := l.now()
	got := map[string]float64{}
	for _, s := range spent {
		lv, ok := l.levels[g.Cred][s.Limit]
		if !ok {
			continue
		}
		got[s.Limit] += s.N
		if s.Remaining >= 0 {
			lv.Remaining = s.Remaining
			lv.ResetAt = s.ResetAt
			if lv.ResetAt.IsZero() {
				lv.ResetAt = now.Add(time.Hour) // the upstream's window is at most an hour
			}
		}
	}
	for name, lim := range lims {
		lv := l.levels[g.Cred][name]
		refill(lv, lim, now)
		lv.Tokens += g.Est[name] - got[name]
	}
	l.save()
}

// Levels is every bucket's state, refilled to now, for a heartbeat.
func (l *Ledger) Levels() map[string]map[string]Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	out := map[string]map[string]Level{}
	for cred, lims := range l.cfg.Credentials {
		out[cred] = map[string]Level{}
		for name, lim := range lims {
			lv := l.levels[cred][name]
			refill(lv, lim, now)
			out[cred][name] = *lv
		}
	}
	return out
}

// Metered says whether cred has a bucket.
func (l *Ledger) Metered(cred string) bool {
	_, ok := l.cfg.Credentials[cred]
	return ok
}

// LimitNames is cred's limit names, sorted.
func (l *Ledger) LimitNames(cred string) []string {
	var out []string
	for n := range l.cfg.Credentials[cred] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// save writes the ledger atomically (temp file + rename). A failed write is
// not fatal: the next one retries, and a restart from an older ledger only
// errs toward more tokens by the unsaved spend of the last few rounds.
func (l *Ledger) save() {
	if l.path == "" {
		return
	}
	raw, err := json.Marshal(l.levels)
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, l.path)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
