package model

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { x := t0.Add(d); return &x }

// One row per end and its boundary: exactly at the end time ends, one second before does not. RED if a boundary
// moves (< vs <=), renew stops moving the expiry, the all-closed end fires without PRs, or none ends.
func TestEndReasonTable(t *testing.T) {
	created := t0.Add(-30 * 24 * time.Hour)
	for _, c := range []struct {
		name string
		stop StopConditions
		prs  int
		all  *time.Time // AllClosedSince
		now  time.Time
		want string
	}{
		{"until: one second before", StopConditions{Deadline: at(0)}, 0, nil, t0.Add(-time.Second), ""},
		{"until: exactly at", StopConditions{Deadline: at(0)}, 0, nil, t0, "until 2026-11-19T12:00:00Z passed"},
		{"expires: one second before 30 d", StopConditions{ExpiresAfterSec: 30 * 86400}, 0, nil, t0.Add(-time.Second), ""},
		{"expires: exactly 30 d after created", StopConditions{ExpiresAfterSec: 30 * 86400}, 0, nil, t0, "expired: 720h0m0s since created"},
		{"renewed a day ago: not yet", StopConditions{ExpiresAfterSec: 30 * 86400, RenewedAt: at(-24 * time.Hour)}, 0, nil, t0, ""},
		{"renewed exactly 30 d ago", StopConditions{ExpiresAfterSec: 30 * 86400, RenewedAt: at(-30 * 24 * time.Hour)}, 0, nil, t0, "expired: 720h0m0s since renewed"},
		{"all closed: grace not over", StopConditions{UntilAllClosed: true, ArchiveGraceSec: 7200}, 2, at(-2*time.Hour + time.Second), t0, ""},
		{"all closed: grace over exactly", StopConditions{UntilAllClosed: true, ArchiveGraceSec: 7200}, 2, at(-2 * time.Hour), t0, "all PRs closed + 2h0m0s grace"},
		{"all closed without PRs never ends", StopConditions{UntilAllClosed: true}, 0, at(-time.Hour), t0, ""},
		{"no end never ends", StopConditions{}, 0, nil, t0.Add(1000 * 24 * time.Hour), ""},
		{"earliest wins: until before expiry", StopConditions{Deadline: at(-time.Hour), ExpiresAfterSec: 60 * 86400}, 0, nil, t0, "until 2026-11-19T11:00:00Z passed"},
	} {
		w := &Critic{CreatedAt: created, Stop: c.stop, PRs: make([]PRRef, c.prs)}
		w.Runtime.AllClosedSince = c.all
		if got := w.EndReason(c.now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The advance warning: inside 3 days of the expiry, once per expiry (a renewal earns a new one); not before the
// window, not after the end. RED if the window moves off 72 h or a warning repeats for the same expiry.
func TestEndWarningOncePerExpiry(t *testing.T) {
	w := &Critic{CreatedAt: t0, Stop: StopConditions{ExpiresAfterSec: 10 * 86400}}
	exp := t0.Add(10 * 24 * time.Hour)
	if _, due := w.EndWarning(exp.Add(-72*time.Hour - time.Second)); due {
		t.Fatal("warned before the 72 h window")
	}
	got, due := w.EndWarning(exp.Add(-72 * time.Hour))
	if !due || !got.Equal(exp) {
		t.Fatalf("at the window: %v %v", got, due)
	}
	w.Runtime.EndWarnedFor = &got
	if _, due := w.EndWarning(exp.Add(-time.Hour)); due {
		t.Fatal("warned twice for one expiry")
	}
	renewed := exp.Add(-time.Hour)
	w.Stop.RenewedAt = &renewed
	if _, due := w.EndWarning(renewed.Add(10*24*time.Hour - time.Hour)); !due {
		t.Fatal("a renewal's new expiry got no warning")
	}
	if _, due := w.EndWarning(renewed.Add(10 * 24 * time.Hour)); due {
		t.Fatal("warned at the end itself")
	}
	if EndWarnBefore != 72*time.Hour {
		t.Fatalf("EndWarnBefore %s", EndWarnBefore)
	}
}

// Archived critics are purged at purge_at, else 7 d after archiving, else now (no time recorded); a live or an
// already ended critic has no purge. RED if the default moves or an ended critic is purged again.
func TestPurgeDue(t *testing.T) {
	w := &Critic{}
	if _, ok := w.PurgeDue(); ok {
		t.Fatal("a live critic has a purge")
	}
	w.Runtime.Archived = true
	if at, ok := w.PurgeDue(); !ok || !at.IsZero() {
		t.Fatalf("no archive time: %v %v, want due now", at, ok)
	}
	w.Runtime.ArchivedAt = t0
	if at, ok := w.PurgeDue(); !ok || !at.Equal(t0.Add(7*24*time.Hour)) {
		t.Fatalf("default: %v", at)
	}
	w.Runtime.PurgeAt = at(time.Hour)
	if got, _ := w.PurgeDue(); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("purge_at: %v", got)
	}
	w.Runtime.EndedAt = at(0)
	if _, ok := w.PurgeDue(); ok {
		t.Fatal("an ended critic is purged again")
	}
}

func TestEndLabel(t *testing.T) {
	for _, c := range []struct {
		name string
		w    Critic
		want string
	}{
		{"none", Critic{}, "none"},
		{"every end", Critic{CreatedAt: t0, PRs: []PRRef{{}}, Stop: StopConditions{Deadline: at(48 * time.Hour), ExpiresAfterSec: 86400, UntilAllClosed: true, ArchiveGraceSec: 172800}}, "until 2026-11-21,exp 2026-11-20,closed+48h"},
		{"archived shows its purge", Critic{Runtime: Runtime{Archived: true, ArchivedAt: t0}}, "purge 2026-11-26"},
		{"archived without a time", Critic{Runtime: Runtime{Archived: true}}, "purge now"},
	} {
		if got := c.w.EndLabel(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The transient default (no --end): all PRs closed + 6 h, and 7 d from creation, renewable. RED if a number moves.
func TestDefaultStopIsTransient(t *testing.T) {
	s := DefaultStop()
	if !s.UntilAllClosed || s.ArchiveGraceSec != 6*3600 || s.ExpiresAfterSec != 7*86400 || s.WithRole != "" || s.Deadline != nil {
		t.Fatalf("default %+v", s)
	}
	w := &Critic{CreatedAt: t0, Stop: s}
	if got := w.EndReason(t0.Add(7 * 24 * time.Hour)); got != "expired: 168h0m0s since created" {
		t.Fatalf("a defaulted critic at 7 d: %q", got)
	}
}

func TestEndLabelWithRole(t *testing.T) {
	w := &Critic{Stop: StopConditions{WithRole: "@fd3ede9ba8849a0d8477befcb40bfa95"}}
	if got := w.EndLabel(); got != "with-role @fd3ede9b" {
		t.Fatalf("%q", got)
	}
}
