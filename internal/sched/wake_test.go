package sched

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/wake"
	"github.com/gluck/ebac/sdk/model"
)

// keysRun records each round's wake keys (nil: a full round). A round
// returns when the test releases it (hold) or at once.
type keysRun struct {
	mu     sync.Mutex
	rounds [][]string
	hold   chan error // nil: return at once
}

func (k *keysRun) run(ctx context.Context, name string, settle func([]model.Spend)) error {
	k.mu.Lock()
	k.rounds = append(k.rounds, wake.Keys(ctx))
	h := k.hold
	k.mu.Unlock()
	if h != nil {
		return <-h
	}
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

func tickWait(s *Scheduler) { s.Tick(context.Background()); s.Wait() }

// A reported change is written WakeDelay later by a write-only round of the
// due keys only, and the cadence does not move. RED if a wake waits for the
// cadence, moves it, or takes a key not yet due.
func TestAWakeWritesOnlyDueKeysAndLeavesTheCadence(t *testing.T) {
	c := newClock()
	k := &keysRun{}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	tickWait(s)
	next := s.Snapshot()["cache"].Next // jitter: 15 min ahead
	s.Wake("cache", "example-repo-00000002")
	c.add(500 * time.Millisecond)
	s.Wake("cache", "example-repo-00000001")
	tickWait(s)
	if n := len(k.got()); n != 0 {
		t.Fatalf("a wake ran before its delay: %d round(s)", n)
	}
	c.add(500 * time.Millisecond) // key 2 due, key 1 due in 0.5 s
	tickWait(s)
	c.add(500 * time.Millisecond)
	tickWait(s)
	got := k.got()
	if len(got) != 2 || !slices.Equal(got[0], []string{"example-repo-00000002"}) || !slices.Equal(got[1], []string{"example-repo-00000001"}) {
		t.Fatalf("rounds %q, want key 2 then key 1, each when due", got)
	}
	e := s.Snapshot()["cache"]
	if !e.Next.Equal(next) || !e.Last.IsZero() || !e.LastWake.Equal(c.now()) || e.Wake != nil || !e.WakeAt.IsZero() {
		t.Fatalf("after the write-only rounds: next %s (want %s), last %s, last_wake %s, wake %v at %s", e.Next, next, e.Last, e.LastWake, e.Wake, e.WakeAt)
	}
}

// One entity is written at most once per WakeGap, counted from when a round
// took it.
func TestAnEntityIsWrittenAtMostOncePerGap(t *testing.T) {
	c := newClock()
	k := &keysRun{}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	tickWait(s)
	s.Wake("cache", "e")
	c.add(DefaultWakeDelay)
	tickWait(s) // written at t+1s
	s.Wake("cache", "e")
	if at := s.Snapshot()["cache"].WakeAt; !at.Equal(c.now().Add(DefaultWakeGap)) {
		t.Fatalf("the next write of e at %s, want the gap after the last (%s)", at, c.now().Add(DefaultWakeGap))
	}
}

// Review B1, cross-component: watcher and scheduler wired as in ebacd. Edit
// A, edit B, a round takes both, edit B again: a second write-only round
// writes B. RED if the second edit is swallowed until the cadence.
func TestAnEditAfterARoundTookItsKeyGetsASecondRound(t *testing.T) {
	c := newClock()
	k := &keysRun{hold: make(chan error, 4)}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	ff := &oneFollow{on: map[string]func(json.RawMessage){}}
	w := &wake.Watcher{Follow: ff.follow, Wake: s.Wake}
	defer w.Close()
	cr := critic("cache", 30*time.Minute)
	cr.Source = model.SourceGHCache
	cr.Runtime.Children = map[string]string{"A": "@0000000000000000000000000000000a", "B": "@0000000000000000000000000000000b"}
	w.Sync(context.Background(), []*model.Critic{cr})
	waitFor(t, "both follows", func() bool { return w.Following() == 2 })
	tickWait(s)
	edit := `{"patch":{"object":{"Update":{"gh":{"object":{"Set":{"title":"x"}}}}}}}`
	ff.push("@0000000000000000000000000000000a", edit)
	c.add(250 * time.Millisecond)
	ff.push("@0000000000000000000000000000000b", edit)
	c.add(2 * time.Second)
	s.Tick(context.Background()) // takes A and B, runs (held)
	waitFor(t, "the first write-only round", func() bool { return len(k.got()) == 1 })
	c.add(100 * time.Millisecond)
	ff.push("@0000000000000000000000000000000b", edit) // B edited again while the round runs
	k.hold <- nil
	s.Wait()
	c.add(DefaultWakeGap)
	k.hold <- nil
	tickWait(s)
	got := k.got()
	if len(got) != 2 || !slices.Equal(got[0], []string{"A", "B"}) || !slices.Equal(got[1], []string{"B"}) {
		t.Fatalf("rounds %q, want [A B] then [B]", got)
	}
}

// A due cadence round is a full round: it takes every pending key with it.
func TestADueCadenceRoundTakesThePendingWake(t *testing.T) {
	c := newClock()
	k := &keysRun{}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	tickWait(s)
	c.add(15 * time.Minute) // the cadence round is due now
	s.Wake("cache", "example-repo-00000001")
	tickWait(s)
	c.add(time.Minute)
	tickWait(s)
	got := k.got()
	if len(got) != 1 || got[0] != nil {
		t.Fatalf("rounds %q, want one full round", got)
	}
	if e := s.Snapshot()["cache"]; e.Wake != nil || !e.WakeAt.IsZero() {
		t.Fatalf("wake left pending after the full round: %v at %s", e.Wake, e.WakeAt)
	}
}

// Review N1: a write-only round that fails puts its keys back, a gap later.
func TestAFailedWriteOnlyRoundRequeuesItsKeys(t *testing.T) {
	c := newClock()
	k := &keysRun{hold: make(chan error, 1)}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	tickWait(s)
	s.Wake("cache", "e")
	c.add(DefaultWakeDelay)
	k.hold <- errors.New("forge down")
	tickWait(s)
	e := s.Snapshot()["cache"]
	if !slices.Equal(e.Wake, []string{"e"}) || !e.WakeAt.Equal(c.now().Add(DefaultWakeGap)) {
		t.Fatalf("after a failed write-only round: wake %v at %s, want [e] at +gap", e.Wake, e.WakeAt)
	}
}

// A wake of a critic the schedule does not know is dropped, not a panic.
func TestAWakeOfAnUnknownCriticIsIgnored(t *testing.T) {
	c := newClock()
	s := wakeSched(c, t, &keysRun{}, critic("cache", 30*time.Minute))
	tickWait(s)
	s.Wake("nobody", "k")
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
	tickWait(s)
	c.add(DefaultListEvery)
	tickWait(s)
	if lists != 2 {
		t.Fatalf("Watch told %d listing(s), want 2", lists)
	}
}

// oneFollow is a follow that is up at once and pushes what the test sends.
type oneFollow struct {
	mu sync.Mutex
	on map[string]func(json.RawMessage)
}

func (f *oneFollow) follow(_ context.Context, id string, on func(json.RawMessage)) (func(), <-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on[id] = on
	return func() {}, make(chan struct{}), nil
}

func (f *oneFollow) push(id, raw string) {
	f.mu.Lock()
	on := f.on[id]
	f.mu.Unlock()
	on(json.RawMessage(raw))
}

// A change to a key already pending rides its write: the deadline stays.
func TestAPendingKeyKeepsItsDeadline(t *testing.T) {
	c := newClock()
	s := wakeSched(c, t, &keysRun{}, critic("cache", 30*time.Minute))
	tickWait(s)
	s.Wake("cache", "e")
	due := c.now().Add(DefaultWakeDelay)
	c.add(700 * time.Millisecond)
	s.Wake("cache", "e")
	if e := s.Snapshot()["cache"]; !e.WakeAt.Equal(due) || !slices.Equal(e.Wake, []string{"e"}) {
		t.Fatalf("wake %v at %s, want [e] at %s", e.Wake, e.WakeAt, due)
	}
}

// Review r2: a CADENCE round that fails puts back the seat changes it took
// (due ones and future ones), a gap later, not a cadence interval later.
func TestAFailedCadenceRoundRequeuesTheKeysItTook(t *testing.T) {
	c := newClock()
	k := &keysRun{hold: make(chan error, 1)}
	s := wakeSched(c, t, k, critic("cache", 30*time.Minute))
	tickWait(s)
	c.add(15*time.Minute - 500*time.Millisecond)
	s.Wake("cache", "e") // due in 1 s; the cadence round is due in 0.5 s
	c.add(500 * time.Millisecond)
	k.hold <- errors.New("forge down")
	tickWait(s)
	if got := k.got(); len(got) != 1 || got[0] != nil {
		t.Fatalf("rounds %q, want one full round", got)
	}
	e := s.Snapshot()["cache"]
	if !slices.Equal(e.Wake, []string{"e"}) || !e.WakeAt.Equal(c.now().Add(DefaultWakeGap)) {
		t.Fatalf("after a failed cadence round: wake %v at %s, want [e] at +gap (%s)", e.Wake, e.WakeAt, c.now().Add(DefaultWakeGap))
	}
}
