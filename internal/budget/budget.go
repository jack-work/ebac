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
	// Keep is the fraction of the shared pool's burst a class may not spend into.
	Keep map[Class]float64 `json:"keep"`
	// WakeShare is the fraction of each limit (rate and burst) set aside as
	// the wake pool: only wake-critical rounds draw on it, so no scan or
	// backfill debt can defer them. 0 = DefaultWakeShare.
	WakeShare float64 `json:"wake_share,omitempty"`
}

// DefaultWakeShare: a fifth of each limit is wake-critical's own.
const DefaultWakeShare = 0.2

func (c Config) wakeShare() float64 {
	if c.WakeShare > 0 {
		return c.WakeShare
	}
	return DefaultWakeShare
}

// pools splits lim into the shared pool and the wake pool (rate per hour, burst).
func (c Config) pools(lim Limit) (sharedRate, sharedBurst, wakeRate, wakeBurst float64) {
	w := c.wakeShare()
	return lim.PerHour * (1 - w), lim.Burst * (1 - w), lim.PerHour * w, lim.Burst * w
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

// Level is one limit's state, persisted and shown: the shared pool (Tokens)
// and the wake pool (Wake).
type Level struct {
	Tokens    float64   `json:"tokens"`
	Wake      float64   `json:"wake"`
	At        time.Time `json:"at"`
	Remaining int       `json:"remaining"` // account-wide, last reported; -1 = never
	ResetAt   time.Time `json:"reset_at,omitzero"`
}

// Grant is a reservation: settle it once the round has run.
type Grant struct {
	Cred  string
	Class Class
	Est   map[string]float64
	// Wake names the limits this grant drew from the wake pool (the rest
	// came from the shared pool); Settle corrects the pool it drew on.
	Wake map[string]bool
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
			_, sb, _, wb := cfg.pools(lim)
			l.levels[cred][name] = &Level{Tokens: sb, Wake: wb, At: t, Remaining: -1}
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
				lv.Tokens, lv.Wake = 0, 0
			}
		}
		return l, fmt.Errorf("budget ledger %s unreadable, every bucket starts empty: %w", path, err)
	}
	for cred, lims := range saved {
		for name, lv := range lims {
			if cur, ok := l.levels[cred][name]; ok && lv != nil {
				*cur = *lv
				_, sb, _, wb := cfg.pools(l.cfg.Credentials[cred][name])
				cur.Tokens, cur.Wake = math.Min(cur.Tokens, sb), math.Min(cur.Wake, wb)
			}
		}
	}
	return l, nil
}

// refill brings both pools up to now at their rates, each capped at its burst.
func (c Config) refill(lv *Level, lim Limit, now time.Time) {
	if dt := now.Sub(lv.At).Seconds(); dt > 0 {
		sr, sb, wr, wb := c.pools(lim)
		lv.Tokens = math.Min(sb, lv.Tokens+dt*sr/3600)
		lv.Wake = math.Min(wb, lv.Wake+dt*wr/3600)
		lv.At = now
	}
}

// fill is how long a pool at rate per hour takes to gain short tokens.
func fill(short, rate float64) time.Duration {
	if rate <= 0 {
		return time.Hour
	}
	return time.Duration(math.Ceil(short*3600/rate*1e3)) * time.Millisecond
}

// Reserve asks for est (by limit name) on cred for a round of class cl. It
// returns a Grant, or the time to ask again. An unmetered credential, or a
// limit the credential does not meter, is always granted.
//
// A wake-critical round that fits the wake pool draws on it first, so scan
// and backfill debt in the shared pool never defers it; otherwise it draws on
// the shared pool down to zero, and waits only for whichever pool fills
// first. Scan and backfill draw only on the shared pool and leave their keep
// share of it. An estimate above the shared pool's burst asks only for that
// burst, so a large round is not starved forever; settling charges what it
// really spent.
func (l *Ledger) Reserve(cred string, cl Class, est map[string]float64) (Grant, time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	lims, metered := l.cfg.Credentials[cred]
	if !metered {
		return Grant{Cred: cred, Class: cl, Est: est}, time.Time{}, true
	}
	var wait time.Time
	fromWake := map[string]bool{}
	for name, lim := range lims {
		lv := l.levels[cred][name]
		l.cfg.refill(lv, lim, now)
		if lim.Floor > 0 && lv.Remaining >= 0 && lv.Remaining < lim.Floor && lv.ResetAt.After(now) {
			wait = later(wait, lv.ResetAt)
			continue
		}
		e := est[name]
		if e <= 0 {
			continue
		}
		sr, sb, wr, wb := l.cfg.pools(lim)
		need := math.Min(e, sb)
		if cl == WakeCritical {
			fits := e <= wb
			if fits && lv.Wake >= e {
				fromWake[name] = true
				continue
			}
			if lv.Tokens >= need {
				continue
			}
			w := fill(need-lv.Tokens, sr)
			if fits {
				w = min(w, fill(e-lv.Wake, wr))
			}
			wait = later(wait, now.Add(w))
			continue
		}
		if short := need + l.cfg.keep(cl)*sb - lv.Tokens; short > 0 {
			wait = later(wait, now.Add(fill(short, sr)))
		}
	}
	if !wait.IsZero() {
		return Grant{}, wait, false
	}
	for name := range lims {
		if fromWake[name] {
			l.levels[cred][name].Wake -= est[name]
		} else {
			l.levels[cred][name].Tokens -= est[name]
		}
	}
	l.save()
	return Grant{Cred: cred, Class: cl, Est: est, Wake: fromWake}, time.Time{}, true
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
		l.cfg.refill(lv, lim, now)
		if g.Wake[name] {
			lv.Wake += g.Est[name] - got[name]
		} else {
			lv.Tokens += g.Est[name] - got[name]
		}
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
			l.cfg.refill(lv, lim, now)
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
