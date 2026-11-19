package budget

import (
	"fmt"
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

// RED if the class shares, the clamp to burst, the debt or the refill rate change: each row's numbers are written out.
func TestReserveTable(t *testing.T) {
	for _, c := range []struct {
		name    string
		start   float64 // graphql tokens before
		class   Class
		est     float64
		granted bool
		wait    time.Duration // exact, when deferred
		after   float64       // graphql tokens after, when granted
	}{
		{"scan within its share is granted", 10, Scan, 5, true, 0, 5},
		{"wake-critical drains the bucket to zero", 10, WakeCritical, 10, true, 0, 0},
		{"scan does not spend into its 20 percent keep", 10, Scan, 9, false, 90 * time.Second, 0},
		{"scan at exactly its keep is granted", 10, Scan, 8, true, 0, 2},
		{"backfill does not spend into its 50 percent keep", 10, Backfill, 6, false, 90 * time.Second, 0},
		{"backfill at exactly its keep is granted", 10, Backfill, 5, true, 0, 5},
		{"an estimate over the burst asks only the burst", 10, WakeCritical, 15, true, 0, -5},
		{"burst plus one from a full bucket is granted as the burst", 10, WakeCritical, 11, true, 0, -1},
		{"a debt defers even wake-critical until repaid", -5, WakeCritical, 1, false, 540 * time.Second, 0},
		{"an empty bucket defers one token by 90 s", 0, WakeCritical, 1, false, 90 * time.Second, 0},
		{"an unknown class keeps like backfill", 10, Class("bogus"), 6, false, 90 * time.Second, 0},
		{"a zero estimate is granted from an empty bucket", 0, Scan, 0, true, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			l := open(t, ck)
			l.levels[ghe]["graphql"].Tokens = c.start
			g, until, ok := l.Reserve(ghe, c.class, map[string]float64{"graphql": c.est})
			if ok != c.granted {
				t.Fatalf("%s: granted=%v, want %v (until %s)", c.name, ok, c.granted, until)
			}
			if !ok {
				if got := until.Sub(ck.t); got != c.wait {
					t.Fatalf("%s: retry in %s, want %s", c.name, got, c.wait)
				}
				if got := tokens(l, "graphql"); got != c.start {
					t.Fatalf("%s: a deferred reservation spent: %v -> %v", c.name, c.start, got)
				}
				return
			}
			if got := tokens(l, "graphql"); got != c.after {
				t.Fatalf("%s: tokens %v, want %v", c.name, got, c.after)
			}
			if g.Cred != ghe {
				t.Fatalf("%s: grant cred %q", c.name, g.Cred)
			}
		})
	}
}

// RED if refill rate or the burst cap goes: 40/h is one point per 90 s, capped at 10.
func TestRefillIsFortyAnHourCappedAtTheBurst(t *testing.T) {
	ck := newClock()
	l := open(t, ck)
	l.levels[ghe]["graphql"].Tokens = 0
	ck.add(90 * time.Second)
	if got := tokens(l, "graphql"); got != 1 {
		t.Fatalf("after 90 s: %v, want 1", got)
	}
	ck.add(time.Hour)
	if got := tokens(l, "graphql"); got != 10 {
		t.Fatalf("after an hour: %v, want the burst 10", got)
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

// RED if Settle stops returning the unspent estimate, stops charging overspend, or drops the reported remaining.
func TestSettleChargesWhatWasSpent(t *testing.T) {
	for _, c := range []struct {
		name      string
		est, real float64
		after     float64
	}{
		{"under the estimate returns the rest", 5, 2, 8},
		{"over the estimate takes the overspend", 5, 8, 2},
		{"exactly the estimate leaves the reservation", 5, 5, 5},
		{"nothing reported settles at zero spend", 5, 0, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			l := open(t, ck)
			g, _, ok := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": c.est})
			if !ok {
				t.Fatal("reserve")
			}
			var sp []Spend
			if c.real > 0 {
				sp = []Spend{{Limit: "graphql", N: c.real, Remaining: -1}}
			}
			l.Settle(g, sp)
			if got := tokens(l, "graphql"); got != c.after {
				t.Fatalf("%s: %v, want %v", c.name, got, c.after)
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
	if got := tokens(l, "graphql"); got != 10 {
		t.Fatalf("missing ledger starts at %v, want full 10", got)
	}
	g, _, _ := l.Reserve(ghe, WakeCritical, map[string]float64{"graphql": 7})
	l.Settle(g, []Spend{{Limit: "graphql", N: 7, Remaining: 2500}})
	l2, err := Open(cfg(), path, ck.now)
	if err != nil {
		t.Fatal(err)
	}
	if lv := l2.Levels()[ghe]["graphql"]; lv.Tokens != 3 || lv.Remaining != 2500 {
		t.Fatalf("reloaded %+v, want tokens 3 remaining 2500", lv)
	}
	if err := os.WriteFile(path, []byte(`{"github:microsoft.ghe.com":{"graphql":{"tok`), 0o600); err != nil {
		t.Fatal(err)
	}
	l3, err := Open(cfg(), path, ck.now)
	if err == nil {
		t.Fatal("a torn ledger opened without an error")
	}
	if got := tokens(l3, "graphql"); got != 0 {
		t.Fatalf("torn ledger starts at %v, want 0", got)
	}
}

// Real concurrency: 32 callers race for a burst of 10 single points; exactly 10 win.
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
	_, until, ok := l.Reserve(ghe, Scan, map[string]float64{"graphql": 9})
	fmt.Println(ok, until.Sub(ck.t))
	// Output: false 1m30s
}
