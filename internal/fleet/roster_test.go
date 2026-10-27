package fleet

import (
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
