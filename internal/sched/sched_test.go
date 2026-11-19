package sched

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/budget"
	"github.com/gluck/ebac/internal/model"
)

const ghe = "github:microsoft.ghe.com"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newClock() *clock { return &clock{t: time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)} }

func ledger(t *testing.T, c *clock) *budget.Ledger {
	l, err := budget.Open(budget.Config{Credentials: map[string]map[string]budget.Limit{ghe: {
		"graphql": {PerHour: 40, Burst: 10, Floor: 1000}, "rest": {PerHour: 600, Burst: 60}}}}, "", c.now)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func critic(name string, every time.Duration) *model.Critic {
	return &model.Critic{Name: name, Host: "microsoft.ghe.com", Source: model.SourceScout, EverySec: int(every.Seconds())}
}

// fake is a Runner whose rounds the test releases by hand.
type fake struct {
	mu      sync.Mutex
	started []string
	release map[string]chan []budget.Spend
	running atomic.Int32
	maxRun  atomic.Int32
}

func newFake() *fake { return &fake{release: map[string]chan []budget.Spend{}} }

func (f *fake) ch(name string) chan []budget.Spend {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.release[name]
	if !ok {
		c = make(chan []budget.Spend, 1)
		f.release[name] = c
	}
	return c
}

func (f *fake) run(ctx context.Context, name string, settle func([]budget.Spend)) error {
	f.mu.Lock()
	f.started = append(f.started, name)
	f.mu.Unlock()
	n := f.running.Add(1)
	for {
		m := f.maxRun.Load()
		if n <= m || f.maxRun.CompareAndSwap(m, n) {
			break
		}
	}
	defer f.running.Add(-1)
	select {
	case sp := <-f.ch(name):
		settle(sp)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.started {
		if s == name {
			n++
		}
	}
	return n
}

func sched(c *clock, l *budget.Ledger, f *fake, ws ...*model.Critic) *Scheduler {
	return &Scheduler{
		List:   func(context.Context) ([]*model.Critic, error) { return ws, nil },
		Run:    f.run,
		Ledger: l,
		Now:    c.now,
		Jitter: func(d time.Duration) time.Duration { return d / 2 },
	}
}

// waitFor spins (no sleep) until cond holds; the fake's rounds run in goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched() // a yield to the round goroutines, not a timing assumption
	}
}

// RED if the defaults move: pool 4, round deadline 10 min, re-list every minute, start spread 2 min.
func TestDefaultsAreTheStatedNumbers(t *testing.T) {
	if DefaultPool != 4 || DefaultRoundTimeout != 10*time.Minute || DefaultListEvery != time.Minute || StartSpread != 2*time.Minute {
		t.Fatalf("pool %d deadline %s list %s spread %s", DefaultPool, DefaultRoundTimeout, DefaultListEvery, StartSpread)
	}
}

// RED if a restart fires every critic at once: each first round is jittered within min(interval, 2 min).
func TestFirstRoundIsSpreadWithinMinOfIntervalAndTwoMinutes(t *testing.T) {
	c := newClock()
	s := sched(c, ledger(t, c), newFake(), critic("slow", 5*time.Minute), critic("fast", 30*time.Second))
	s.Tick(context.Background())
	snap := s.Snapshot()
	for name, want := range map[string]time.Duration{"slow": time.Minute, "fast": 15 * time.Second} {
		if got := snap[name].Next.Sub(c.now()); got != want {
			t.Errorf("%s first due in %s, want %s", name, got, want)
		}
	}
	if f := s.Run; f == nil {
		t.Fatal("no runner")
	}
}

func TestADueCriticRunsAndIsDueAgainOneIntervalAfterItEnds(t *testing.T) {
	c := newClock()
	f := newFake()
	s := sched(c, ledger(t, c), f, critic("a", 5*time.Minute))
	s.Tick(context.Background())
	if f.count("a") != 0 {
		t.Fatal("ran before it was due")
	}
	c.add(time.Minute)
	s.Tick(context.Background())
	waitFor(t, "a started", func() bool { return f.count("a") == 1 })
	f.ch("a") <- nil
	s.Wait()
	e := s.Snapshot()["a"]
	if e.Running || !e.Next.Equal(c.now().Add(5*time.Minute)) || !e.Last.Equal(c.now()) {
		t.Fatalf("after the round: %+v", e)
	}
}

// RED if the running guard goes: a critic due again while its round runs is not started twice.
func TestACriticNeverRunsTwiceAtOnce(t *testing.T) {
	c := newClock()
	f := newFake()
	s := sched(c, ledger(t, c), f, critic("a", 30*time.Second))
	s.Tick(context.Background()) // lists: due in 15 s
	c.add(15 * time.Second)
	s.Tick(context.Background())
	s.Tick(context.Background())
	waitFor(t, "a started", func() bool { return f.count("a") == 1 })
	c.add(time.Hour)
	s.Tick(context.Background())
	s.Tick(context.Background())
	if n := f.count("a"); n != 1 {
		t.Fatalf("started %d times while running", n)
	}
	f.ch("a") <- nil
	s.Wait()
}

// RED if the pool bound goes: 6 due critics, pool 2, real concurrency: never more than 2 at once, all 6 run.
func TestThePoolBoundsConcurrentRounds(t *testing.T) {
	c := newClock()
	f := newFake()
	var ws []*model.Critic
	for i := 0; i < 6; i++ {
		ws = append(ws, critic(fmt.Sprintf("c%d", i), 30*time.Second))
	}
	s := sched(c, ledger(t, c), f, ws...)
	s.Pool = 2
	s.Tick(context.Background()) // lists: all due in 15 s
	c.add(time.Minute)
	done := 0
	for done < 6 {
		s.Tick(context.Background())
		want := int32(min(2, 6-done))
		waitFor(t, "the pool filled", func() bool { return f.running.Load() == want })
		f.mu.Lock()
		started := append([]string(nil), f.started...)
		f.mu.Unlock()
		for _, n := range started[done:] {
			f.ch(n) <- nil
			done++
		}
		waitFor(t, "rounds drained", func() bool { return f.running.Load() == 0 })
	}
	s.Wait()
	if m := f.maxRun.Load(); m != 2 {
		t.Fatalf("max concurrent %d, want exactly the pool 2", m)
	}
}

// RED if Tick ignores the ledger: an empty bucket defers the round to the refill time, the runner is not called.
func TestAShortBucketDefersTheRoundToItsRefill(t *testing.T) {
	c := newClock()
	l := ledger(t, c)
	if _, _, ok := l.Reserve(ghe, budget.WakeCritical, map[string]float64{"graphql": 10}); !ok {
		t.Fatal("drain")
	}
	f := newFake()
	s := sched(c, l, f, critic("a", 5*time.Minute)) // scan: keeps 2, asks 1; a minute refills 2/3 -> short 7/3 -> 210 s
	s.Tick(context.Background())                    // lists: due in 1 min
	c.add(time.Minute)
	s.Tick(context.Background())
	if f.count("a") != 0 {
		t.Fatal("ran on an empty bucket")
	}
	e := s.Snapshot()["a"]
	if got := e.Next.Sub(c.now()); got != 210*time.Second || !strings.Contains(e.Deferred, "short") {
		t.Fatalf("deferred %s (%q), want 210s", got, e.Deferred)
	}
}

func TestStoppedArchivedAndRemovedCriticsAreNotScheduled(t *testing.T) {
	c := newClock()
	stopped, archived := critic("stopped", time.Minute), critic("archived", time.Minute)
	stopped.Runtime.Stopped, archived.Runtime.Archived = true, true
	live := critic("live", time.Minute)
	ws := []*model.Critic{stopped, archived, live}
	s := sched(c, ledger(t, c), newFake(), ws...)
	s.List = func(context.Context) ([]*model.Critic, error) { return ws, nil }
	s.Tick(context.Background())
	snap := s.Snapshot()
	if _, ok := snap["live"]; !ok || len(snap) != 1 {
		t.Fatalf("scheduled %v, want only live", keys(snap))
	}
	ws = nil // removed
	c.add(DefaultListEvery)
	s.Tick(context.Background())
	if len(s.Snapshot()) != 0 {
		t.Fatalf("removed critic still scheduled: %v", keys(s.Snapshot()))
	}
}

// RED if a hung round keeps its slot, or is run again while hung.
func TestAHungRoundReleasesItsSlotAndIsNotRunAgain(t *testing.T) {
	c := newClock()
	f := newFake()
	hang := make(chan struct{})
	var cancels []context.CancelFunc
	var cmu sync.Mutex
	s := sched(c, ledger(t, c), f, critic("hung", time.Minute), critic("next", time.Minute))
	s.Pool = 1
	s.Jitter = func(time.Duration) time.Duration { return 0 }
	s.Deadline = func(ctx context.Context) (context.Context, context.CancelFunc) {
		x, cancel := context.WithCancel(ctx)
		cmu.Lock()
		cancels = append(cancels, cancel)
		cmu.Unlock()
		return x, cancel
	}
	s.Run = func(ctx context.Context, name string, settle func([]budget.Spend)) error {
		if name == "hung" {
			f.mu.Lock()
			f.started = append(f.started, name)
			f.mu.Unlock()
			<-hang // ignores its context: truly hung
			return nil
		}
		return f.run(ctx, name, settle)
	}
	s.Tick(context.Background())
	waitFor(t, "hung started", func() bool { return f.count("hung") == 1 })
	if f.count("next") != 0 {
		t.Fatal("pool of 1 ran two rounds")
	}
	cmu.Lock()
	cancels[0]() // the deadline passes
	cmu.Unlock()
	waitFor(t, "hung marked", func() bool { return s.Snapshot()["hung"].Hung })
	c.add(time.Hour)
	s.Tick(context.Background())
	waitFor(t, "next started in the released slot", func() bool { return f.count("next") == 1 })
	if f.count("hung") != 1 {
		t.Fatal("a hung critic was started again")
	}
	f.ch("next") <- nil
	close(hang)
	s.Wait()
	if e := s.Snapshot()["hung"]; e.Hung || e.Running || !strings.Contains(e.Err, "deadline") {
		t.Fatalf("after the hung round returned: %+v", e)
	}
}

// RED if spend is not settled, or the estimate stops following it: est 1, spent 5 -> next est 3; tokens 10 - 5 = 5.
func TestARoundSettlesItsSpendAndTheEstimateFollows(t *testing.T) {
	c := newClock()
	l := ledger(t, c)
	f := newFake()
	s := sched(c, l, f, critic("a", time.Minute))
	s.Jitter = func(time.Duration) time.Duration { return 0 }
	s.Tick(context.Background())
	waitFor(t, "a started", func() bool { return f.count("a") == 1 })
	f.ch("a") <- []budget.Spend{{Limit: "graphql", N: 5, Remaining: 4000}}
	s.Wait()
	if got := l.Levels()[ghe]["graphql"]; got.Tokens != 5 || got.Remaining != 4000 {
		t.Fatalf("bucket %+v, want tokens 5 remaining 4000", got)
	}
	if est := s.Snapshot()["a"].Est; est["graphql"] != 3 || est["rest"] != 1 {
		t.Fatalf("estimate %v, want graphql 3 rest 1", est)
	}
}

// One row per source: the credential seam and the class table, literal.
func TestCredentialAndClassBySource(t *testing.T) {
	for _, c := range []struct {
		source string
		mode   model.Mode
		cred   string
		class  budget.Class
	}{
		{model.SourceApprove, "", ghe, budget.WakeCritical},
		{model.SourceOrgApprove, "", ghe, budget.WakeCritical},
		{"github-pr", model.ModeReviewer, ghe, budget.WakeCritical},
		{"github-pr", model.ModeReview, ghe, budget.Scan},
		{model.SourceScout, "", ghe, budget.Scan},
		{"github-author", "", ghe, budget.Backfill},
		{model.SourceBandHub, "", ghe, budget.Backfill},
		{"teams-chat", "", "teams", budget.WakeCritical},
		{"keel-threads", "", "", budget.WakeCritical},
		{model.SourceKeelBranches, "", "", budget.Backfill},
		{model.SourceRoster, "", "", budget.Backfill},
		{model.SourceReleases, "", "", budget.Backfill},
	} {
		w := &model.Critic{Source: c.source, Mode: c.mode, Host: "microsoft.ghe.com"}
		if got := Credential(w); got != c.cred {
			t.Errorf("%s/%s: credential %q, want %q", c.source, c.mode, got, c.cred)
		}
		if got := ClassOf(w); got != c.class {
			t.Errorf("%s/%s: class %q, want %q", c.source, c.mode, got, c.class)
		}
	}
	w := &model.Critic{Source: model.SourceScout, Priority: "backfill"}
	if ClassOf(w) != budget.Backfill {
		t.Error("a critic's own priority must win")
	}
	if got := Credential(&model.Critic{Source: "github-pr"}); got != "github:github.com" {
		t.Errorf("no host: %q, want github:github.com", got)
	}
}

func keys(m map[string]Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
