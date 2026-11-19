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

// RED if the defaults move: pool 4, round deadline 10 min, re-list every minute.
func TestDefaultsAreTheStatedNumbers(t *testing.T) {
	if DefaultPool != 4 || DefaultRoundTimeout != 10*time.Minute || DefaultListEvery != time.Minute {
		t.Fatalf("pool %d deadline %s list %s", DefaultPool, DefaultRoundTimeout, DefaultListEvery)
	}
}

// A newly seen critic keeps its cadence when its last round is less than an interval old; an overdue one (never
// polled, or its interval passed while ebacd was down) runs at a jittered moment within ONE interval. RED if the
// spread shrinks back to a fixed window (a boot stampede) or the cadence is lost.
func TestFirstRoundKeepsCadenceOrSpreadsOverdueOverOneInterval(t *testing.T) {
	c := newClock()
	recent := critic("recent", 5*time.Minute)
	recent.Runtime.LastPolled = c.now().Add(-time.Minute)
	stale := critic("stale", 30*time.Minute)
	stale.Runtime.LastPolled = c.now().Add(-6 * time.Hour)
	s := sched(c, ledger(t, c), newFake(), recent, stale, critic("never", 5*time.Minute))
	s.Tick(context.Background())
	snap := s.Snapshot()
	for name, want := range map[string]time.Duration{"recent": 4 * time.Minute, "stale": 15 * time.Minute, "never": 150 * time.Second} {
		if got := snap[name].Next.Sub(c.now()); got != want {
			t.Errorf("%s first due in %s, want %s", name, got, want)
		}
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
	c.add(150 * time.Second)
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
	s.Tick(context.Background()) // lists: never polled, due within its 30 s interval (jitter d/2 = 15 s)
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
	s.Tick(context.Background()) // lists: all due in 15 s (jitter d/2 of 30 s)
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
		// Drained = the SCHEDULER has released every slot, not just the fake's counter: the fake's round returns
		// before the scheduler's finish frees the slot, and a Tick in between would start nothing.
		waitFor(t, "rounds drained", func() bool {
			for _, e := range s.Snapshot() {
				if e.Running {
					return false
				}
			}
			return true
		})
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
	s := sched(c, l, f, critic("a", 5*time.Minute)) // shared pool 8 drained to -2; 150 s refill 4/3 -> -2/3; scan keeps 1.6, asks 1 -> short 49/15 -> 367.5 s
	s.Tick(context.Background())                    // lists: due in 2.5 min (jitter d/2 of 5 min)
	c.add(150 * time.Second)
	s.Tick(context.Background())
	if f.count("a") != 0 {
		t.Fatal("ran on an empty bucket")
	}
	e := s.Snapshot()["a"]
	if got := e.Next.Sub(c.now()); got != 367500*time.Millisecond || !strings.Contains(e.Deferred, "short") {
		t.Fatalf("deferred %s (%q), want 367.5s", got, e.Deferred)
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

// RED if spend is not settled, or the estimate stops following it: est 1, spent 5 -> next est 3; shared 8 - 5 = 3.
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
	if got := l.Levels()[ghe]["graphql"]; got.Tokens != 3 || got.Wake != 2 || got.Remaining != 4000 {
		t.Fatalf("bucket %+v, want shared 3 wake 2 remaining 4000", got)
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

type endCall struct{ name, reason string }

// Ends are enforced before scheduling: a reached end is ended once (and never reserved), an ended critic has its
// kills retried, an archived critic at its purge date is purged, a running critic is left for the next listing, and
// an expiry inside 72 h is warned. RED if any arm goes.
func TestEndsAreEnforcedBeforeScheduling(t *testing.T) {
	c := newClock()
	now := c.now()
	past := now.Add(-time.Second)
	reached := critic("reached", time.Minute)
	reached.Stop.Deadline = &past
	ended := critic("ended", time.Minute)
	ended.Runtime.Archived, ended.Runtime.EndedAt = true, &past
	purge := critic("purge", time.Minute)
	purge.Runtime.Archived, purge.Runtime.ArchivedAt = true, now.Add(-8*24*time.Hour)
	young := critic("young-archive", time.Minute)
	young.Runtime.Archived, young.Runtime.ArchivedAt = true, now.Add(-time.Hour)
	warn := critic("warn", time.Minute)
	warn.CreatedAt, warn.Stop.ExpiresAfterSec = now.Add(-28*24*time.Hour), 30*86400
	live := critic("live", time.Minute)
	f := newFake()
	s := sched(c, ledger(t, c), f, reached, ended, purge, young, warn, live)
	var calls []endCall
	var warned []string
	s.End = func(_ context.Context, w *model.Critic, reason string) error {
		calls = append(calls, endCall{w.Name, reason})
		if reason != "" && w.Runtime.EndedAt == nil {
			w.Runtime.Archived, w.Runtime.EndedAt = true, &now
		}
		return nil
	}
	s.Warn = func(_ context.Context, w *model.Critic, at time.Time) error {
		warned = append(warned, w.Name)
		return nil
	}
	s.Tick(context.Background())
	got := map[string]string{}
	for _, e := range calls {
		got[e.name] = e.reason
	}
	if len(calls) != 3 || got["reached"] != "until "+past.UTC().Format(time.RFC3339)+" passed" || got["ended"] != "" ||
		got["purge"] != "purge: archived "+purge.Runtime.ArchivedAt.UTC().Format(time.RFC3339) {
		t.Fatalf("end calls %+v", calls)
	}
	if _, ok := got["young-archive"]; ok {
		t.Fatal("an archive inside its 7 d was purged")
	}
	if len(warned) != 1 || warned[0] != "warn" {
		t.Fatalf("warned %v, want [warn]", warned)
	}
	snap := s.Snapshot()
	if _, ok := snap["reached"]; ok {
		t.Fatal("an ended critic was scheduled")
	}
	if _, ok := snap["live"]; !ok {
		t.Fatal("control: the live critic is not scheduled")
	}
}

// A with-role critic: an ABSORBED role is followed (every address repointed to the absorber, the critic kept, not
// ended); a RETIRED role ends it; an unreadable role ends nothing; a standing role keeps it as is. RED if absorption
// ends a critic, a retirement does not, or an unreadable role ends one.
func TestWithRoleFollowsAbsorptionEndsOnRetirement(t *testing.T) {
	c := newClock()
	mk := func(name, role string) *model.Critic {
		w := critic(name, time.Minute)
		w.Stop = model.StopConditions{WithRole: role}
		return w
	}
	absorbed := mk("absorbed", "@old")
	s := sched(c, ledger(t, c), newFake(), mk("held", "@held"), mk("retired", "@gone"), mk("unknown", "@err"), absorbed)
	var ended []endCall
	var repointed []string
	s.End = func(_ context.Context, w *model.Critic, reason string) error {
		ended = append(ended, endCall{w.Name, reason})
		w.Runtime.Archived = true
		return nil
	}
	s.Follow = func(_ context.Context, id string) (string, bool, string, error) {
		switch id {
		case "@gone":
			return id, true, "retired: its form is gone", nil
		case "@err":
			return "", false, "", fmt.Errorf("role %s unreadable: connection closed", id)
		case "@old":
			return "@new", false, "", nil
		}
		return id, false, "", nil
	}
	s.Repoint = func(_ context.Context, w *model.Critic, from, to string) error {
		repointed = append(repointed, w.Name+" "+from+"->"+to)
		return nil
	}
	s.Tick(context.Background())
	if len(ended) != 1 || ended[0] != (endCall{"retired", "role @gone retired: its form is gone"}) {
		t.Fatalf("ended %+v, want only retired", ended)
	}
	if len(repointed) != 1 || repointed[0] != "absorbed @old->@new" || absorbed.Stop.WithRole != "@new" {
		t.Fatalf("repointed %v (with_role now %q), want absorbed @old->@new", repointed, absorbed.Stop.WithRole)
	}
	snap := s.Snapshot()
	for _, n := range []string{"held", "unknown", "absorbed"} {
		if _, ok := snap[n]; !ok {
			t.Errorf("%s is not kept scheduled", n)
		}
	}
}

// A critic with no trusted authority is never scheduled (fail closed). RED if it is.
func TestNoAuthorityIsNotScheduled(t *testing.T) {
	c := newClock()
	none, ok := critic("none", time.Minute), critic("ok", time.Minute)
	none.NoAuthority = true
	s := sched(c, ledger(t, c), newFake(), none, ok)
	s.Tick(context.Background())
	snap := s.Snapshot()
	if _, in := snap["none"]; in || len(snap) != 1 {
		t.Fatalf("scheduled %v", keys(snap))
	}
}
