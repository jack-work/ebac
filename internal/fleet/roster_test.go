package fleet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// A live critic whose last round is older than 4 intervals is late (ebacd not scheduling it, or its rounds not
// ending); exactly 4 intervals is on time; never polled, stopped or archived is not late. RED if LateFactor or the
// comparison moves.
func TestLateCriticIsNeverHealthy(t *testing.T) {
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	row := RosterRow{Seat: "abc123"}
	for _, c := range []struct {
		name    string
		ago     time.Duration // since LastPolled; 0 = never polled
		stopped bool
		health  string
	}{
		{"exactly four 5-minute intervals is on time", 20 * time.Minute, false, "ok"},
		{"one second past four intervals is late", 20*time.Minute + time.Second, false, "late: last round 20m1s ago, polls every 5m0s"},
		{"never polled is not late", 0, false, "ok"},
		{"stopped is not late", time.Hour, true, "stopped: "},
	} {
		w := &model.Critic{Name: "x", EverySec: 300}
		if c.ago > 0 {
			w.Runtime.LastPolled = now.Add(-c.ago)
		}
		w.Runtime.Stopped = c.stopped
		if got := health(w, row, now); got != c.health {
			t.Errorf("%s: %q, want %q", c.name, got, c.health)
		}
	}
}

func TestUnreachableSeatIsNeverHealthy(t *testing.T) {
	for _, seat := range []string{"(empty)", "abc123 (unreachable)"} {
		if got := health(&model.Critic{Name: "x"}, RosterRow{Seat: seat}, time.Now()); got == "ok" {
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
	if w := keel("@alert"); health(w, row(w), time.Now()) != "ok" {
		t.Fatalf("--alert critic with a live alert role: %q (seat %q)", health(w, row(w), time.Now()), row(w).Seat)
	}
	if w := keel("@void"); health(w, row(w), time.Now()) != "no seat cast" {
		t.Fatalf("empty alert role: %q", health(w, row(w), time.Now()))
	}
	if w := keel("@stale"); health(w, row(w), time.Now()) == "ok" {
		t.Fatalf("dead alert holder scored healthy")
	}
	if w := keel(""); health(w, row(w), time.Now()) != "no seat cast" {
		t.Fatalf("control: a seatless critic without --alert: %q", health(w, row(w), time.Now()))
	}
}
