package fleet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// Regression: the roster's health column scored an unarmed critic as "ok"
// because it compared TimerState against the literal "inactive/disabled",
// while systemd actually says "not-found" for a unit that was never
// installed. The selfcheck disagreed with the roster about one fact. Health
// must be derived from the PROPERTY, so no unrecognised display string can
// ever read as healthy.
func TestUnarmedTimerIsNeverHealthy(t *testing.T) {
	w := &model.Critic{Name: "x"}
	row := RosterRow{Seat: "abc123"}
	for _, ts := range []systemd.TimerStatus{
		{Display: "absent"},
		{Display: "disabled", Known: true},
		{Display: "enabled/inactive", Known: true, Enabled: true},
		{Display: "something-systemd-invents-in-2027"},
	} {
		if got := health(w, row, ts); got == "ok" {
			t.Errorf("timer %+v scored healthy", ts)
		}
	}
	armed := systemd.TimerStatus{Active: true, Enabled: true, Known: true, Display: "armed"}
	if got := health(w, row, armed); got != "ok" {
		t.Errorf("armed timer scored %q, want ok", got)
	}
}

func TestUnreachableSeatIsNeverHealthy(t *testing.T) {
	armed := systemd.TimerStatus{Active: true, Enabled: true, Known: true, Display: "armed"}
	for _, seat := range []string{"(empty)", "abc123 (unreachable)"} {
		if got := health(&model.Critic{Name: "x"}, RosterRow{Seat: seat}, armed); got == "ok" {
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
	armed := systemd.TimerStatus{Active: true, Enabled: true, Known: true, Display: "armed"}
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
	if w := keel("@alert"); health(w, row(w), armed) != "ok" {
		t.Fatalf("--alert critic with a live alert role: %q (seat %q)", health(w, row(w), armed), row(w).Seat)
	}
	if w := keel("@void"); health(w, row(w), armed) != "no seat cast" {
		t.Fatalf("empty alert role: %q", health(w, row(w), armed))
	}
	if w := keel("@stale"); health(w, row(w), armed) == "ok" {
		t.Fatalf("dead alert holder scored healthy")
	}
	if w := keel(""); health(w, row(w), armed) != "no seat cast" {
		t.Fatalf("control: a seatless critic without --alert: %q", health(w, row(w), armed))
	}
}
