package budget

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ghe = "github:microsoft.ghe.com"

// cfg is the production numbers (ebac-steward 11-19): GraphQL 40 points/h, burst 10, floor 1000; REST 600/h, burst 60.
func cfg() Config {
	return Config{Credentials: map[string]map[string]Limit{ghe: {
		"graphql": {PerHour: 40, Burst: 10, Floor: 1000},
		"rest":    {PerHour: 600, Burst: 60},
	}}}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)} }
func open(t *testing.T, c *clock) *Ledger {
	l, err := Open(cfg(), "", c.now)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func tokens(l *Ledger, limit string) float64 { return l.Levels()[ghe][limit].Tokens }

// The production numbers split: wake pool = a fifth (burst 2, 8/h: one token per 450 s), shared pool = the rest
// (burst 8, 32/h: one token per 112.5 s). RED if the class shares, the wake pool, the clamp or a refill rate change:
// each row's numbers are written out.
func TestReserveTable(t *testing.T) {
	for _, c := range []struct {
		name         string
		shared, wake float64 // tokens before
		class        Class
		est          float64
		granted      bool
		wait         time.Duration // exact, when deferred
		sharedAfter  float64
		wakeAfter    float64
	}{
		{"scan within its share is granted", 8, 2, Scan, 5, true, 0, 3, 2},
		{"scan does not spend into its keep (1.6 of 8)", 8, 2, Scan, 7, false, 67500 * time.Millisecond, 8, 2},
		{"scan at exactly its keep is granted", 8, 2, Scan, 6.4, true, 0, 1.6, 2},
		{"backfill does not spend into its keep (4 of 8)", 8, 2, Backfill, 5, false, 112500 * time.Millisecond, 8, 2},
		{"backfill at exactly its keep is granted", 8, 2, Backfill, 4, true, 0, 4, 2},
		{"scan never draws on the wake pool", 0, 2, Scan, 1, false, 292500 * time.Millisecond, 0, 2},
		{"wake-critical draws its own pool while the shared pool is in debt", -5, 2, WakeCritical, 1, true, 0, -5, 1},
		{"wake-critical borrows the shared pool when its own is short", 8, 0, WakeCritical, 1, true, 0, 7, 0},
		{"wake-critical waits for whichever pool fills first", -5, 0, WakeCritical, 1, false, 450 * time.Second, -5, 0},
		{"wake-critical over the wake burst uses the shared pool", 8, 2, WakeCritical, 3, true, 0, 5, 2},
		{"an estimate over the shared burst asks only that burst", 8, 2, WakeCritical, 15, true, 0, -7, 2},
		{"an empty bucket defers wake-critical by the shared refill", 0, 0, WakeCritical, 1, false, 112500 * time.Millisecond, 0, 0},
		{"an unknown class keeps like backfill", 8, 2, Class("bogus"), 5, false, 112500 * time.Millisecond, 8, 2},
		{"a zero estimate is granted from empty pools", 0, 0, Scan, 0, true, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			l := open(t, ck)
			lv := l.levels[ghe]["graphql"]
			lv.Tokens, lv.Wake = c.shared, c.wake
			g, until, ok := l.Reserve(ghe, c.class, map[string]float64{"graphql": c.est})
			if ok != c.granted {
				t.Fatalf("%s: granted=%v, want %v (until %s)", c.name, ok, c.granted, until)
			}
			if !ok {
				if got := until.Sub(ck.t); got != c.wait {
					t.Fatalf("%s: retry in %s, want %s", c.name, got, c.wait)
				}
			} else if g.Cred != ghe || g.Class != c.class {
				t.Fatalf("%s: grant %+v", c.name, g)
			}
			got := l.Levels()[ghe]["graphql"]
			if math.Abs(got.Tokens-c.sharedAfter) > 1e-9 || math.Abs(got.Wake-c.wakeAfter) > 1e-9 {
				t.Fatalf("%s: shared %v wake %v, want %v %v", c.name, got.Tokens, got.Wake, c.sharedAfter, c.wakeAfter)
			}
		})
	}
}

// RED if a refill rate or a cap goes: shared 32/h (one per 112.5 s) capped at 8, wake 8/h (one per 450 s) capped at 2.
func TestRefillFillsBothPoolsAtTheirRatesCappedAtTheirBursts(t *testing.T) {
	ck := newClock()
	l := open(t, ck)
	lv := l.levels[ghe]["graphql"]
	lv.Tokens, lv.Wake = 0, 0
	ck.add(450 * time.Second)
	if got := l.Levels()[ghe]["graphql"]; got.Tokens != 4 || got.Wake != 1 {
		t.Fatalf("after 450 s: shared %v wake %v, want 4 1", got.Tokens, got.Wake)
	}
	ck.add(time.Hour)
	if got := l.Levels()[ghe]["graphql"]; got.Tokens != 8 || got.Wake != 2 {
		t.Fatalf("after an hour: shared %v wake %v, want the bursts 8 2", got.Tokens, got.Wake)
	}
	if DefaultWakeShare != 0.2 {
		t.Fatalf("wake share %v", DefaultWakeShare)
	}
}

// RED if the floor goes, or compares with <= instead of <.
func TestFloorDefersEveryClassUntilTheReset(t *testing.T) {
	for _, c := range []struct {
		name      string
		remaining int
		resetIn   time.Duration
		granted   bool
	}{
		{"remaining 999 under the floor defers to the reset", 999, 10 * time.Minute, false},
		{"remaining exactly the floor 1000 is granted", 1000, 10 * time.Minute, true},
		{"under the floor but past the reset is granted", 999, -time.Second, true},
		{"never reported (-1) is granted", -1, 10 * time.Minute, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			l := open(t, ck)
			lv := l.levels[ghe]["graphql"]
			lv.Remaining, lv.ResetAt = c.remaining, ck.t.Add(c.resetIn)
			_, until, ok := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": 1})
			if ok != c.granted {
				t.Fatalf("%s: granted=%v, want %v", c.name, ok, c.granted)
			}
			if !ok && !until.Equal(ck.t.Add(c.resetIn)) {
				t.Fatalf("%s: retry at %s, want the reset %s", c.name, until, ck.t.Add(c.resetIn))
			}
		})
	}
}

// The floor holds a round that asks nothing of the floored limit too: the account is shared, every GitHub read waits.
func TestFloorDefersARoundThatAsksOnlyREST(t *testing.T) {
	ck := newClock()
	l := open(t, ck)
	lv := l.levels[ghe]["graphql"]
	lv.Remaining, lv.ResetAt = 10, ck.t.Add(time.Minute)
	if _, _, ok := l.Reserve(ghe, WakeCritical, map[string]float64{"rest": 1}); ok {
		t.Fatal("a REST-only round was granted under the GraphQL floor")
	}
}

func TestUnmeteredCredentialIsAlwaysGranted(t *testing.T) {
	l := open(t, newClock())
	if _, _, ok := l.Reserve("teams", Backfill, map[string]float64{"graphql": 1e9}); !ok {
		t.Fatal("unmetered credential deferred")
	}
	if _, _, ok := l.Reserve(ghe, Backfill, map[string]float64{"graphql": 1e9}); ok {
		t.Fatal("control: the metered credential granted a huge backfill estimate")
	}
}

// RED if Settle stops returning the unspent estimate, stops charging overspend, or corrects the wrong pool.
func TestSettleChargesThePoolTheGrantDrewOn(t *testing.T) {
	for _, c := range []struct {
		name         string
		class        Class
		est, real    float64
		shared, wake float64 // after
	}{
		{"scan under the estimate returns the rest", Scan, 5, 2, 6, 2},
		{"scan over the estimate takes the overspend", Scan, 5, 8, 0, 2},
		{"scan exactly the estimate", Scan, 5, 5, 3, 2},
		{"nothing reported settles at zero spend", Scan, 5, 0, 8, 2},
		{"a wake-pool grant settles in the wake pool", WakeCritical, 1, 3, 8, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			l := open(t, ck)
			g, _, ok := l.Reserve(ghe, c.class, map[string]float64{"graphql": c.est})
			if !ok {
				t.Fatal("reserve")
			}
			var sp []Spend
			if c.real > 0 {
				sp = []Spend{{Limit: "graphql", N: c.real, Remaining: -1}}
			}
			l.Settle(g, sp)
			if got := l.Levels()[ghe]["graphql"]; got.Tokens != c.shared || got.Wake != c.wake {
				t.Fatalf("%s: shared %v wake %v, want %v %v", c.name, got.Tokens, got.Wake, c.shared, c.wake)
			}
		})
	}
}

func TestSettleRecordsTheAccountRemainingAndItsReset(t *testing.T) {
	ck := newClock()
	l := open(t, ck)
	g, _, _ := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": 1})
	reset := ck.t.Add(17 * time.Minute)
	l.Settle(g, []Spend{{Limit: "graphql", N: 1, Remaining: 4321, ResetAt: reset}})
	if lv := l.Levels()[ghe]["graphql"]; lv.Remaining != 4321 || !lv.ResetAt.Equal(reset) {
		t.Fatalf("level %+v", lv)
	}
	l.Settle(g, []Spend{{Limit: "graphql", N: 1, Remaining: 900}})
	if lv := l.Levels()[ghe]["graphql"]; lv.Remaining != 900 || !lv.ResetAt.Equal(ck.t.Add(time.Hour)) {
		t.Fatalf("a remaining without a reset must assume the hour window: %+v", lv)
	}
	l.Settle(g, []Spend{{Limit: "graphql", N: 1, Remaining: -1}})
	if lv := l.Levels()[ghe]["graphql"]; lv.Remaining != 900 {
		t.Fatalf("an unreported remaining (-1) overwrote the last report: %+v", lv)
	}
}

// RED if the ledger stops persisting, or a torn file starts the buckets full.
func TestLedgerPersistsAndATornFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	ck := newClock()
	l, err := Open(cfg(), path, ck.now)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if lv := l.Levels()[ghe]["graphql"]; lv.Tokens != 8 || lv.Wake != 2 {
		t.Fatalf("missing ledger starts at %+v, want full 8 2", lv)
	}
	g, _, _ := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": 7}) // over the wake burst: shared 8 -> 1
	l.Settle(g, []Spend{{Limit: "graphql", N: 7, Remaining: 2500}})
	l2, err := Open(cfg(), path, ck.now)
	if err != nil {
		t.Fatal(err)
	}
	if lv := l2.Levels()[ghe]["graphql"]; lv.Tokens != 1 || lv.Wake != 2 || lv.Remaining != 2500 {
		t.Fatalf("reloaded %+v, want shared 1 wake 2 remaining 2500", lv)
	}
	if err := os.WriteFile(path, []byte(`{"github:microsoft.ghe.com":{"graphql":{"tok`), 0o600); err != nil {
		t.Fatal(err)
	}
	l3, err := Open(cfg(), path, ck.now)
	if err == nil {
		t.Fatal("a torn ledger opened without an error")
	}
	if lv := l3.Levels()[ghe]["graphql"]; lv.Tokens != 0 || lv.Wake != 0 {
		t.Fatalf("torn ledger starts at %+v, want 0 0", lv)
	}
}

// Real concurrency: 32 wake-critical callers race for 2 wake + 8 shared single points; exactly 10 win.
func TestConcurrentReservationsNeverOverspend(t *testing.T) {
	l := open(t, newClock())
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": 1}); ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 10 {
		t.Fatalf("%d grants from a burst of 10", wins.Load())
	}
}

func ExampleLedger_Reserve() {
	ck := newClock()
	l, _ := Open(cfg(), "", ck.now)
	_, until, ok := l.Reserve(ghe, Scan, map[string]float64{"graphql": 7})
	fmt.Println(ok, until.Sub(ck.t))
	// Output: false 1m7.5s
}
