package fleet

import (
	"context"
	"fmt"
	"github.com/gluck/ebac/internal/sched"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// Health reads ebacd's heartbeat, never last_polled (a quiet round persists nothing, so last_polled lags by hours on
// a healthy critic: 11-19, three critics read "late" while polling). Late = the heartbeat's last round older than 4
// intervals; exactly 4 is on time; a critic this ebacd has not run yet, stopped or archived is not late; no
// heartbeat, or one older than twice its period, is the scheduler's fault and says so. RED if health goes back to
// last_polled, or LateFactor or the stale bound moves.
func TestHealthReadsTheHeartbeat(t *testing.T) {
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	row := RosterRow{Seat: "abc123"}
	fresh := func(last time.Duration) *sched.Beat {
		b := &sched.Beat{At: now.Add(-30 * time.Second), EveryS: 60, Critics: map[string]sched.Entry{}}
		if last >= 0 {
			b.Critics["x"] = sched.Entry{Last: now.Add(-last)}
		}
		return b
	}
	for _, c := range []struct {
		name    string
		beat    *sched.Beat
		stopped bool
		polled  time.Duration // last_polled age: must not matter
		health  string
	}{
		{"exactly four 5-minute intervals is on time", fresh(20 * time.Minute), false, 0, "ok"},
		{"one second past four intervals is late", fresh(20*time.Minute + time.Second), false, 0, "late: last round 20m1s ago, polls every 5m0s"},
		{"a stale last_polled with a recent heartbeat round is ok", fresh(time.Minute), false, 5 * time.Hour, "ok"},
		{"not yet run by this ebacd is not late", fresh(-1), false, 5 * time.Hour, "ok"},
		{"stopped is not late", fresh(time.Hour), true, 0, "stopped: "},
		{"no heartbeat says so", nil, false, 0, "no ebacd heartbeat (is ebacd running?)"},
		{"a heartbeat two periods old is still fresh", &sched.Beat{At: now.Add(-2 * time.Minute), EveryS: 60}, false, 0, "ok"},
		{"a heartbeat past two periods is a dead scheduler", &sched.Beat{At: now.Add(-2*time.Minute - time.Second), EveryS: 60}, false, 0, "ebacd heartbeat 2m1s old: scheduler down"},
	} {
		w := &model.Critic{Name: "x", EverySec: 300}
		if c.polled > 0 {
			w.Runtime.LastPolled = now.Add(-c.polled)
		}
		w.Runtime.Stopped = c.stopped
		if got := health(w, row, now, c.beat); got != c.health {
			t.Errorf("%s: %q, want %q", c.name, got, c.health)
		}
	}
	if LateFactor != 4 {
		t.Fatalf("LateFactor %d", LateFactor)
	}
}

func TestUnreachableSeatIsNeverHealthy(t *testing.T) {
	for _, seat := range []string{"(empty)", "abc123 (unreachable)"} {
		if got := health(&model.Critic{Name: "x"}, RosterRow{Seat: seat}, time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}); got == "ok" {
			t.Errorf("seat %q scored healthy", seat)
		}
	}
}

func snapWith(prs ...*model.PRState) *model.Snapshot {
	s := &model.Snapshot{PRs: map[model.PRKey]*model.PRState{}, Complete: true, TakenAt: time.Now()}
	for _, p := range prs {
		if p.Threads == nil {
			p.Threads = map[string]*model.Thread{}
		}
		if p.Reviews == nil {
			p.Reviews = map[string]*model.Review{}
		}
		p.Complete = true
		s.PRs[p.Key] = p
	}
	return s
}

type fakeSeats struct {
	forms map[string]map[string]any
	alive map[string]bool
}

func (f fakeSeats) Form(_ context.Context, id string) (map[string]any, error) {
	if m, ok := f.forms[id]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("no form %s", id)
}

func (f fakeSeats) AriaExists(_ context.Context, id string) bool { return f.alive[id] }

// A keel critic added with --alert @ROLE has no seat on its own form by design: it is seated (healthy) when the
// alert role resolves to a live aria, and "no seat cast" / unreachable only when the ALERT role is empty / dead.
// Control: a critic without --alert is still judged by its own form.
func TestAlertCriticSeatIsTheAlertRole(t *testing.T) {
	ctx := context.Background()
	fig := fakeSeats{
		forms: map[string]map[string]any{
			"@own":   {},                          // the critic's own role form: empty by design
			"@alert": {"target-aria": "holder01"}, // the role it wakes
			"@void":  {},                          // an alert role nobody holds
			"@stale": {"target-aria": "gone0001"}, // an alert role whose holder is gone
		},
		alive: map[string]bool{"holder01": true},
	}
	keel := func(alert string) *model.Critic {
		return &model.Critic{Name: "keel-x", Source: "keel-threads", FormID: "@own", Keel: &model.KeelRef{AlertRole: alert}}
	}
	row := func(w *model.Critic) RosterRow {
		seat, ok, err := SeatOf(ctx, fig, w)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case seat == "":
			seat = "(empty)"
		case !ok:
			seat += " (unreachable)"
		}
		return RosterRow{Seat: seat}
	}
	if w := keel("@alert"); health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}) != "ok" {
		t.Fatalf("--alert critic with a live alert role: %q (seat %q)", health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}), row(w).Seat)
	}
	if w := keel("@void"); health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}) != "no seat cast" {
		t.Fatalf("empty alert role: %q", health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}))
	}
	if w := keel("@stale"); health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}) == "ok" {
		t.Fatalf("dead alert holder scored healthy")
	}
	if w := keel(""); health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}) != "no seat cast" {
		t.Fatalf("control: a seatless critic without --alert: %q", health(w, row(w), time.Now(), &sched.Beat{At: time.Now(), EveryS: 60}))
	}
}
