package callpace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a shared mock clock: Sleep advances it (no wall-clock sleeps).
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

var t0 = time.Date(2026, 11, 25, 16, 40, 0, 0, time.UTC)

func ledger(path string, c *clock, note *strings.Builder) *Ledger {
	l := &Ledger{Path: path, Limit: Limit, Window: Window, Now: c.Now, Sleep: c.Sleep}
	if note != nil {
		l.Note = note
	}
	return l
}

// maxInWindow is the most points booked in any Window starting at a booking.
func maxInWindow(at []time.Time) int {
	most := 0
	for _, s := range at {
		n := 0
		for _, u := range at {
			if !u.Before(s) && u.Before(s.Add(Window)) {
				n++
			}
		}
		most = max(most, n)
	}
	return most
}

// The incident's batch (#76): 75 comment edits of 2 points each (a read and the mutation). Every edit completes (wait,
// never fail), no 5-minute window holds more than 30 points, and the 150 points take exactly 4 windows to start:
// 30 at 16:40, 16:45, 16:50, 16:55 and 17:00. RED without the pacing (all 150 at 16:40).
func TestTheIncidentBatchNeverSpendsMoreThanThirtyPointsInFiveMinutes(t *testing.T) {
	c := &clock{now: t0}
	l := ledger(filepath.Join(t.TempDir(), FileName), c, nil)
	var at []time.Time
	for edit := 0; edit < 75; edit++ {
		for call := 0; call < 2; call++ {
			if err := l.Wait(context.Background()); err != nil {
				t.Fatalf("edit %d call %d: %v", edit, call, err)
			}
			at = append(at, c.Now())
		}
	}
	if len(at) != 150 {
		t.Fatalf("%d calls made, want 150", len(at))
	}
	if got := maxInWindow(at); got != 30 {
		t.Fatalf("most points in a 5-min window = %d, want 30", got)
	}
	if want := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC); !at[149].Equal(want) {
		t.Fatalf("the last call ran at %s, want %s", at[149], want)
	}
	if !at[29].Equal(t0) || !at[30].Equal(t0.Add(Window)) {
		t.Fatalf("call 30 at %s, call 31 at %s: want %s then %s", at[29], at[30], t0, t0.Add(Window))
	}
}

// A call that cost more than its reserved point books the rest: a 5-point read, 25 one-point calls and a 3-point
// settle on the last (33 points), and the 27th call waits exactly until the 5-point read leaves the window (it alone
// frees enough: the wait weighs points, not calls). RED if Settle books nothing or the wait counts calls.
func TestSettleBooksWhatACallCostBeyondItsPoint(t *testing.T) {
	c := &clock{now: t0}
	l := ledger(filepath.Join(t.TempDir(), FileName), c, nil)
	ctx := context.Background()
	if err := l.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	l.Settle(4)
	c.now = t0.Add(time.Second)
	for i := 0; i < 25; i++ {
		if err := l.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Now().Equal(t0.Add(time.Second)) {
		t.Fatalf("the 25 calls waited until %s", c.Now())
	}
	l.Settle(3) // the newest call cost 4: the window now holds 33, over the limit; the 5-point read still frees enough
	if err := l.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(Window); !c.Now().Equal(want) {
		t.Fatalf("the 27th call ran at %s, want %s", c.Now(), want)
	}
}

// Two processes (two ledgers on one file, two goroutines at once) share the one budget: 30 calls each, never more
// than 30 points in a window between them. RED if the file is not locked or not shared.
func TestTwoProcessesShareOneBudget(t *testing.T) {
	c := &clock{now: t0}
	path := filepath.Join(t.TempDir(), FileName)
	var mu sync.Mutex
	var at []time.Time
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for p := 0; p < 2; p++ {
		l := ledger(path, c, nil)
		l.Booked = func(t time.Time) { mu.Lock(); at = append(at, t); mu.Unlock() }
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if err := l.Wait(context.Background()); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(at) != 60 {
		t.Fatalf("%d calls, want 60", len(at))
	}
	if got := maxInWindow(at); got > 30 {
		t.Fatalf("%d points in one 5-min window across both processes, want <= 30", got)
	}
}

// A wait whose context ends returns that error (the only way Wait fails).
func TestAWaitWhoseContextEndsReturnsItsError(t *testing.T) {
	c := &clock{now: t0}
	l := ledger(filepath.Join(t.TempDir(), FileName), c, nil)
	for i := 0; i < 30; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A wait is said once per wait, with the window's spend.
func TestAWaitIsSaid(t *testing.T) {
	c := &clock{now: t0}
	var note strings.Builder
	l := ledger(filepath.Join(t.TempDir(), FileName), c, &note)
	for i := 0; i < 31; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if want := "ebac: call pace: 30 of 30 points spent in the last 5m0s; waiting 5m0s\n"; note.String() != want {
		t.Fatalf("note = %q, want %q", note.String(), want)
	}
}

// A budget file the process cannot open paces nothing: the call goes ahead (never fail) and the note says why.
// RED if an unusable budget fails the edit.
func TestAnUnusableBudgetLetsTheCallGoAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	block := filepath.Join(dir, "file")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: t0}
	var note strings.Builder
	l := ledger(filepath.Join(block, FileName), c, &note) // a path under a regular file
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !strings.HasPrefix(note.String(), "ebac: call pace: budget "+filepath.Join(block, FileName)+" unusable, not paced: ") {
		t.Fatalf("note = %q", note.String())
	}
}

// A budget file that does not parse starts empty (and says so) rather than blocking every call.
func TestAnUnreadableBudgetStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: t0}
	var note strings.Builder
	l := ledger(path, c, &note)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Now().Equal(t0) {
		t.Fatalf("waited until %s", c.Now())
	}
	if !strings.HasPrefix(note.String(), "ebac: call pace: budget "+path+" unreadable, starting it empty: ") {
		t.Fatalf("note = %q", note.String())
	}
}
