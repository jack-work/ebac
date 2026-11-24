// Package sched is ebacd's scheduler: it runs every live critic's round at
// its interval in one long-running process (it replaces one systemd timer
// per critic). Each round first reserves its estimated spend on the critic's
// credential from the budget ledger; a short bucket defers the round to the
// bucket's refill time. A bounded pool runs the rounds, a critic never runs
// twice at once, and a round past its deadline gives its pool slot back.
package sched

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/gluck/ebac/internal/budget"
	"github.com/gluck/ebac/sdk/model"
)

// Defaults. Each is pinned by a test that states the number.
const (
	DefaultPool         = 4
	DefaultRoundTimeout = 10 * time.Minute
	// DefaultListEvery is how often the critic set is re-read: a new critic
	// starts within this, a removed one stops.
	DefaultListEvery = time.Minute
	// defaultEst is the first estimate of a critic never settled.
	defaultEst = 1
)

// Runner runs one round of the named critic. settle is called with what the
// round spent (it may be called more than once: once per plugin call).
type Runner func(ctx context.Context, name string, settle func([]model.Spend)) error

// Entry is one critic's place in the schedule, as the heartbeat shows it.
type Entry struct {
	Next    time.Time `json:"next"`
	Last    time.Time `json:"last,omitzero"`
	Running bool      `json:"running,omitempty"`
	// Hung: the round passed its deadline and has not returned; its slot
	// is back in the pool, the critic is not run again until it returns.
	Hung     bool               `json:"hung,omitempty"`
	Deferred string             `json:"deferred,omitempty"` // why the last reservation waited
	Err      string             `json:"err,omitempty"`
	Est      map[string]float64 `json:"est,omitempty"`
	Cred     string             `json:"cred,omitempty"`
	Class    budget.Class       `json:"class,omitempty"`
	interval time.Duration
}

// Scheduler runs the rounds. Fields are set before Run; zero values take
// the defaults.
type Scheduler struct {
	List   func(ctx context.Context) ([]*model.Critic, error)
	Run    Runner
	Ledger *budget.Ledger
	Now    func() time.Time
	// Jitter returns a duration in [0, d): the start spread (rand in
	// production, fixed in tests).
	Jitter       func(d time.Duration) time.Duration
	Pool         int
	RoundTimeout time.Duration
	ListEvery    time.Duration
	Out          io.Writer
	// Deadline gives a round its context (nil: RoundTimeout from now);
	// tests cancel it by hand instead of waiting on a clock.
	Deadline func(ctx context.Context) (context.Context, context.CancelFunc)
	// End ends a critic whose end is reached (reason), or retries the kills
	// of one already ended (reason ""); Warn sends the advance warning of an
	// expiry. nil: ends are not enforced (tests of the schedule alone).
	End  func(ctx context.Context, w *model.Critic, reason string) error
	Warn func(ctx context.Context, w *model.Critic, at time.Time) error
	// Follow is a with-role critic's role now (fleet.FollowRole): followed
	// along moved-to when absorbed, retired when gone; an error means
	// unknown, and nothing ends. Repoint moves every address of an absorbed
	// role on that critic to the absorbing role (fleet.Repoint, Only).
	Follow  func(ctx context.Context, id string) (current string, retired bool, why string, err error)
	Repoint func(ctx context.Context, w *model.Critic, from, to string) error

	mu       sync.Mutex
	entries  map[string]*Entry
	listed   time.Time
	slots    int // in use
	inflight sync.WaitGroup
}

func (s *Scheduler) defaults() {
	if s.Pool <= 0 {
		s.Pool = DefaultPool
	}
	if s.RoundTimeout <= 0 {
		s.RoundTimeout = DefaultRoundTimeout
	}
	if s.ListEvery <= 0 {
		s.ListEvery = DefaultListEvery
	}
	if s.Out == nil {
		s.Out = io.Discard
	}
	if s.Jitter == nil {
		s.Jitter = func(time.Duration) time.Duration { return 0 }
	}
	if s.entries == nil {
		s.entries = map[string]*Entry{}
	}
}

// Tick is one pass: re-list when due, then start every due critic a pool
// slot and its budget allow. It never blocks on a round.
func (s *Scheduler) Tick(ctx context.Context) {
	s.mu.Lock()
	s.defaults()
	now := s.Now()
	needList := s.listed.IsZero() || now.Sub(s.listed) >= s.ListEvery
	s.mu.Unlock()
	if needList {
		ws, err := s.List(ctx)
		if err != nil {
			fmt.Fprintf(s.Out, "ebacd: list critics: %v\n", err)
		} else {
			s.ends(ctx, ws, now)
			s.sync(ws, now)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.dueNames(now) {
		if s.slots >= s.Pool {
			return
		}
		e := s.entries[name]
		g, until, ok := s.Ledger.Reserve(e.Cred, e.Class, e.Est)
		if !ok {
			e.Next = until
			e.Deferred = fmt.Sprintf("budget %s short at %s, retry %s", e.Cred, now.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
			fmt.Fprintf(s.Out, "%s: reserve %s %v (%s) deferred until %s\n", name, e.Cred, e.Est, e.Class, until.UTC().Format(time.RFC3339))
			continue
		}
		e.Deferred = ""
		e.Running = true
		s.slots++
		s.inflight.Add(1)
		go s.round(ctx, name, g)
	}
}

// ends enforces every critic's end before it is scheduled: a critic whose end
// is reached is ended (and leaves the schedule), one ended earlier whose forms
// are not all gone has its kills retried, and an expiry inside the warning
// window is announced once. A critic whose round is running is left for the
// next listing.
func (s *Scheduler) ends(ctx context.Context, ws []*model.Critic, now time.Time) {
	if s.End == nil {
		return
	}
	for _, w := range ws {
		s.mu.Lock()
		running := s.entries[w.Name] != nil && s.entries[w.Name].Running
		s.mu.Unlock()
		if running {
			continue
		}
		switch {
		case w.Runtime.EndedAt != nil:
			if err := s.End(ctx, w, ""); err != nil {
				fmt.Fprintf(s.Out, "%s: %v\n", w.Name, err)
			}
		case w.Runtime.Archived:
			if at, due := w.PurgeDue(); due && !now.Before(at) {
				if err := s.End(ctx, w, "purge: archived "+w.Runtime.ArchivedAt.UTC().Format(time.RFC3339)); err != nil {
					fmt.Fprintf(s.Out, "%s: %v\n", w.Name, err)
				}
			}
		case w.Runtime.Stopped:
		case w.Stop.WithRole != "":
			if s.Follow == nil {
				continue
			}
			cur, retired, why, err := s.Follow(ctx, w.Stop.WithRole)
			switch {
			case err != nil:
				fmt.Fprintf(s.Out, "%s: with-role end not judged: %v\n", w.Name, err)
			case retired:
				if err := s.End(ctx, w, "role "+w.Stop.WithRole+" "+why); err != nil {
					fmt.Fprintf(s.Out, "%s: %v\n", w.Name, err)
				}
			case cur != w.Stop.WithRole:
				// Absorbed: the critic follows its role, it does not end.
				if s.Repoint == nil {
					continue
				}
				if err := s.Repoint(ctx, w, w.Stop.WithRole, cur); err != nil {
					fmt.Fprintf(s.Out, "%s: role %s absorbed into %s, repoint failed (retried next listing): %v\n", w.Name, w.Stop.WithRole, cur, err)
					continue
				}
				fmt.Fprintf(s.Out, "%s: role %s absorbed into %s: repointed, critic kept\n", w.Name, w.Stop.WithRole, cur)
				w.Stop.WithRole = cur
			}
		default:
			if reason := w.EndReason(now); reason != "" {
				if err := s.End(ctx, w, reason); err != nil {
					fmt.Fprintf(s.Out, "%s: %v\n", w.Name, err)
				}
				continue
			}
			if at, due := w.EndWarning(now); due && s.Warn != nil {
				if err := s.Warn(ctx, w, at); err != nil {
					fmt.Fprintf(s.Out, "%s: end warning: %v\n", w.Name, err)
				}
			}
		}
	}
}

// sync makes the entries the live critic set: new critics are due within the
// start spread, removed, stopped or archived ones leave, a changed interval
// or class takes effect at the next round.
func (s *Scheduler) sync(ws []*model.Critic, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = now
	seen := map[string]bool{}
	for _, w := range ws {
		if w.Runtime.Stopped || w.Runtime.Archived || w.NoAuthority {
			continue // a critic with no trusted authority is not run (#11)
		}
		seen[w.Name] = true
		e, ok := s.entries[w.Name]
		if !ok {
			e = &Entry{Next: firstDue(w, now, s.Jitter), Est: map[string]float64{}}
			s.entries[w.Name] = e
		}
		e.interval, e.Cred, e.Class = w.Interval(), Credential(w), ClassOf(w)
		for _, lim := range s.Ledger.LimitNames(e.Cred) {
			if _, ok := e.Est[lim]; !ok {
				e.Est[lim] = defaultEst
			}
		}
	}
	for name, e := range s.entries {
		if !seen[name] && !e.Running {
			delete(s.entries, name)
		}
	}
}

// firstDue is when a critic newly seen (at start, or newly added) runs first:
// one interval after its last round if that is still ahead, so a restart keeps
// its cadence; an OVERDUE critic (never polled, or its interval passed while
// ebacd was down) at a jittered moment within one interval, so after a long
// outage the overdue rounds spread out instead of draining the budget at once.
func firstDue(w *model.Critic, now time.Time, jitter func(time.Duration) time.Duration) time.Time {
	if next := w.Runtime.LastPolled.Add(w.Interval()); !w.Runtime.LastPolled.IsZero() && next.After(now) {
		return next
	}
	return now.Add(jitter(w.Interval()))
}

// dueNames is the critics due now and not running, most overdue first.
func (s *Scheduler) dueNames(now time.Time) []string {
	var out []string
	for name, e := range s.entries {
		if !e.Running && !e.Next.After(now) {
			out = append(out, name)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := s.entries[out[i]], s.entries[out[j]]
		if !a.Next.Equal(b.Next) {
			return a.Next.Before(b.Next)
		}
		return out[i] < out[j]
	})
	return out
}

// round runs one critic's round under its deadline. Past the deadline the
// pool slot is released and the critic marked hung; it stays out of the
// schedule until the round returns.
func (s *Scheduler) round(ctx context.Context, name string, g budget.Grant) {
	defer s.inflight.Done()
	dl := s.Deadline
	if dl == nil {
		dl = func(c context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(c, s.RoundTimeout)
		}
	}
	rctx, cancel := dl(ctx)
	defer cancel()
	var spentMu sync.Mutex
	var spent []model.Spend
	done := make(chan error, 1)
	go func() {
		done <- s.Run(rctx, name, func(sp []model.Spend) {
			spentMu.Lock()
			spent = append(spent, sp...)
			spentMu.Unlock()
		})
	}()
	var err error
	select {
	case err = <-done:
	case <-rctx.Done():
		if ctx.Err() != nil {
			// ebacd is stopping, not a hung round: wait for it like any other.
			s.finish(name, g, spent, &spentMu, <-done, true)
			return
		}
		s.mu.Lock()
		if e := s.entries[name]; e != nil {
			e.Hung = true
		}
		s.slots--
		s.mu.Unlock()
		fmt.Fprintf(s.Out, "%s: round passed its %s deadline: killed, pool slot released\n", name, s.RoundTimeout)
		err = <-done // the runner's own context is cancelled: wait for it to return
		s.finish(name, g, spent, &spentMu, fmt.Errorf("deadline %s: %v", s.RoundTimeout, err), false)
		return
	}
	s.finish(name, g, spent, &spentMu, err, true)
}

func (s *Scheduler) finish(name string, g budget.Grant, spent []model.Spend, mu *sync.Mutex, err error, releaseSlot bool) {
	mu.Lock()
	sp := append([]model.Spend(nil), spent...)
	mu.Unlock()
	s.Ledger.Settle(g, sp)
	s.mu.Lock()
	defer s.mu.Unlock()
	if releaseSlot {
		s.slots--
	}
	e := s.entries[name]
	if e == nil {
		return
	}
	now := s.Now()
	e.Running, e.Hung, e.Last = false, false, now
	e.Next = now.Add(e.interval)
	e.Err = ""
	if err != nil {
		e.Err = model.FirstLine(err.Error())
		fmt.Fprintf(s.Out, "%s: %v\n", name, err)
	}
	// The next estimate is this round's spend, smoothed: half the last
	// estimate, half what was spent (never below the default).
	got := map[string]float64{}
	for _, x := range sp {
		got[x.Limit] += x.N
	}
	for _, lim := range s.Ledger.LimitNames(e.Cred) {
		prev, ok := e.Est[lim]
		if !ok {
			prev = defaultEst
		}
		e.Est[lim] = max(defaultEst, (prev+got[lim])/2)
	}
}

// Snapshot is every entry, for the heartbeat.
func (s *Scheduler) Snapshot() map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Entry{}
	for n, e := range s.entries {
		c := *e
		c.Est = map[string]float64{}
		for k, v := range e.Est {
			c.Est[k] = v
		}
		out[n] = c
	}
	return out
}

// Wait blocks until every started round has returned.
func (s *Scheduler) Wait() { s.inflight.Wait() }

// Credential is the one place that knows which upstream credential a critic
// spends: the budget is per credential because the upstream's quota is per
// account. "" is unmetered.
func Credential(w *model.Critic) string {
	switch w.SourceName() {
	case model.SourceReleases, model.SourceRoster, model.SourceKeelBranches, "keel-threads":
		return "" // public github.com, figaro, keel: no shared account quota
	case "teams-chat", model.SourceTeamsLive:
		return "teams"
	}
	host := w.Host
	if host == "" {
		host = "github.com"
	}
	return "github:" + host
}

// ClassOf is a critic's priority class: its own priority if set, else by
// what it serves (a waiting seat or asker first, scans next, backfill last).
func ClassOf(w *model.Critic) budget.Class {
	if w.Priority != "" {
		return budget.Class(w.Priority)
	}
	switch w.SourceName() {
	case model.SourceApprove, model.SourceOrgApprove, "teams-chat", "keel-threads":
		return budget.WakeCritical
	case model.SourceScout:
		return budget.Scan
	case model.SourceBandHub, model.SourceRoster, model.SourceReleases, model.SourceKeelBranches, "github-author":
		return budget.Backfill
	}
	if w.Mode == model.ModeReviewer {
		return budget.WakeCritical
	}
	return budget.Scan
}
