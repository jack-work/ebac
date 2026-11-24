package sched

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/wake"
	"github.com/gluck/ebac/sdk/model"
)

// keysRun records each round's wake keys (nil: a full round) and returns at once.
type keysRun struct {
	mu     sync.Mutex
	rounds [][]string
}

func (k *keysRun) run(ctx context.Context, name string, settle func([]model.Spend)) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rounds = append(k.rounds, wake.Keys(ctx))
	return nil
}

func (k *keysRun) got() [][]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([][]string(nil), k.rounds...)
}

func wakeSched(c *clock, t *testing.T, k *keysRun, ws ...*model.Critic) *Scheduler {
	s := sched(c, ledger(t, c), newFake(), ws...)
	s.Run = k.run
	return s
}

// A wake runs a write-only round of the woken entities at its time, and the
// cadence does not move: the next full round is still one interval after
// the last one. RED if a wake waits for the cadence or moves it.
func TestAWakeRunsAWriteOnlyRoundAndLeavesTheCadence(t *testing.T) {
	c := newClock()
	k := &keysRun{}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	s.Tick(context.Background())
	next := s.Snapshot()["cache"].Next // jitter: 15 min ahead
	s.Wake("cache", "example-repo-00000002", c.now().Add(time.Second))
	s.Wake("cache", "example-repo-00000001", c.now().Add(3*time.Second))
	s.Tick(context.Background())
	if n := len(k.got()); n != 0 {
		t.Fatalf("a wake 1 s ahead ran at once: %d round(s)", n)
	}
	c.add(time.Second)
	s.Tick(context.Background())
	s.Wait()
	got := k.got()
	if len(got) != 1 || !slices.Equal(got[0], []string{"example-repo-00000001", "example-repo-00000002"}) {
		t.Fatalf("rounds %q, want one write-only round of both keys", got)
	}
	e := s.Snapshot()["cache"]
	if !e.Next.Equal(next) || !e.Last.IsZero() || !e.LastWake.Equal(c.now()) || e.Wake != nil || !e.WakeAt.IsZero() {
		t.Fatalf("after the write-only round: next %s (want %s), last %s, last_wake %s, wake %v at %s", e.Next, next, e.Last, e.LastWake, e.Wake, e.WakeAt)
	}
}

// A due cadence round is a full round: it carries the pending wake away
// (keys cleared, no write-only round after it). RED if both run.
func TestADueCadenceRoundTakesThePendingWake(t *testing.T) {
	c := newClock()
	k := &keysRun{}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	s.Tick(context.Background())
	c.add(15 * time.Minute) // the cadence round is due now
	s.Wake("cache", "example-repo-00000001", c.now())
	s.Tick(context.Background())
	s.Wait()
	s.Tick(context.Background())
	s.Wait()
	got := k.got()
	if len(got) != 1 || got[0] != nil {
		t.Fatalf("rounds %q, want one full round", got)
	}
	if e := s.Snapshot()["cache"]; e.Wake != nil || !e.WakeAt.IsZero() {
		t.Fatalf("wake left pending after the full round: %v at %s", e.Wake, e.WakeAt)
	}
}

// A wake asked while the critic's round runs runs after it, not never.
func TestAWakeDuringARoundRunsAfterIt(t *testing.T) {
	c := newClock()
	f := newFake()
	s := sched(c, ledger(t, c), f, critic("cache", 30*time.Minute))
	s.Tick(context.Background())
	c.add(15 * time.Minute)
	s.Tick(context.Background())
	waitFor(t, "the full round started", func() bool { return f.count("cache") == 1 })
	s.Wake("cache", "example-repo-00000001", c.now())
	f.ch("cache") <- nil
	s.Wait()
	s.Tick(context.Background())
	waitFor(t, "the write-only round started", func() bool { return f.count("cache") == 2 })
	f.ch("cache") <- nil
	s.Wait()
}

// A wake of a critic the schedule does not know is dropped, not a panic.
func TestAWakeOfAnUnknownCriticIsIgnored(t *testing.T) {
	c := newClock()
	s := wakeSched(c, t, &keysRun{}, critic("cache", 30*time.Minute))
	s.Tick(context.Background())
	s.Wake("nobody", "k", c.now())
	if _, ok := s.Snapshot()["nobody"]; ok {
		t.Fatal("an unknown critic entered the schedule")
	}
}

// Watch is told every listing (the entity forms to follow).
func TestWatchIsToldEveryListing(t *testing.T) {
	c := newClock()
	s := wakeSched(c, t, &keysRun{}, critic("cache", 30*time.Minute))
	var lists int
	s.Watch = func(_ context.Context, ws []*model.Critic) {
		if len(ws) == 1 && ws[0].Name == "cache" {
			lists++
		}
	}
	s.Tick(context.Background())
	c.add(DefaultListEvery)
	s.Tick(context.Background())
	if lists != 2 {
		t.Fatalf("Watch told %d listing(s), want 2", lists)
	}
}
