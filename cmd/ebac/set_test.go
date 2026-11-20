package main

import (
	"context"
	"encoding/json"
	"github.com/gluck/ebac/internal/livetest"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/model"
)

func TestCheckPingEvery(t *testing.T) {
	every := time.Minute
	for ping, ok := range map[time.Duration]bool{0: true, time.Minute: true, 30 * time.Minute: true, 30 * time.Second: false, -time.Second: false} {
		if err := checkPingEvery(ping, every); (err == nil) != ok {
			t.Errorf("ping %s every %s: err %v, want ok %v", ping, every, err, ok)
		}
	}
}

func TestDescribeCollect(t *testing.T) {
	w := &model.Critic{}
	if got := describeCollect(w); !strings.Contains(got, "collect off") {
		t.Fatalf("off: %q", got)
	}
	w.PingEverySec = 1800
	w.Runtime.LastPing = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	if got := describeCollect(w); got != "collect mode, ping every 30m0s, last ping 2026-11-10T18:00:00Z" {
		t.Fatalf("on: %q", got)
	}
}

// Live: ebac set --ping-every lands in the critic key on a real role form, and 0 turns it off; the rest of the
// critic is untouched.
func TestLiveSetPingEvery(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.FormNew(ctx, "ebac-livetest-set")
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, f, id)
	name := "livetest-set-" + strings.TrimPrefix(id, "@")[:8]
	w := &model.Critic{Name: name, Mode: model.ModeTeams, Source: "teams-chat", FormID: id, EverySec: 60, CreatedAt: time.Now().UTC(),
		Chat: &model.ChatRef{ID: "48:notes", Topic: "t"}}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := cmdSet(ctx, globals{}, []string{"--critic", name, "--ping-every", "30m"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadCritic(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if got.PingEverySec != 1800 || got.Chat == nil || got.Chat.ID != "48:notes" || got.EverySec != 60 {
		t.Fatalf("after set: ping %d chat %+v every %d", got.PingEverySec, got.Chat, got.EverySec)
	}
	if err := cmdSet(ctx, globals{}, []string{"--critic", name, "--ping-every", "30s"}); err == nil {
		t.Fatal("a ping shorter than the poll interval was accepted")
	}
	if err := cmdSet(ctx, globals{}, []string{"--critic", name, "--ping-every", "0"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LoadCritic(ctx, name); got.PingEverySec != 0 {
		t.Fatalf("0 did not turn collect mode off: %d", got.PingEverySec)
	}
}

func TestDescribeLimit(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	s := &model.ReleasesState{LimitedUntil: now.Add(30 * time.Minute)}
	if got := describeLimit(s, now); !strings.Contains(got, "RATE LIMITED by GitHub until 2026-11-10T20:30:00Z") || !strings.Contains(got, "not a failure") {
		t.Fatalf("limited: %q", got)
	}
	n := 41
	s = &model.ReleasesState{LimitedUntil: now.Add(-time.Minute), Remaining: &n}
	if got := describeLimit(s, now); got != "41 request(s) left this hour (unauthenticated, shared egress IP)" {
		t.Fatalf("after the reset: %q", got)
	}
}

// ebac set --every: the interval lands in every_sec (ebacd takes it at the next round); too short, under a keel feed's floor, or above the collect ping is refused and changes nothing.
func TestSetEvery(t *testing.T) {
	w := &model.Critic{Name: "author-jack", EverySec: 120}
	if err := setEvery(w, 5*time.Minute, 0); err != nil || w.EverySec != 300 {
		t.Fatalf("5m: err %v every_sec %d", err, w.EverySec)
	}
	for _, c := range []struct {
		w     *model.Critic
		every time.Duration
		ping  time.Duration
	}{
		{&model.Critic{EverySec: 60}, 5 * time.Second, 0},
		{&model.Critic{EverySec: 60}, 10 * time.Minute, 5 * time.Minute},
		{&model.Critic{EverySec: 60, Keel: &model.KeelRef{Feed: true}}, 15 * time.Second, 0},
	} {
		if err := setEvery(c.w, c.every, c.ping); err == nil || c.w.EverySec != 60 {
			t.Errorf("every %s ping %s keel %v: accepted (every_sec %d)", c.every, c.ping, c.w.Keel, c.w.EverySec)
		}
	}
}

// --end replaces the critic's end; each spec arm sets exactly its field; "none" is refused alone or beside another end
// (no critic lives forever, the Boss 11-19); bad specs change nothing. RED if an arm sets the wrong field, none is
// accepted anywhere, or a bad spec half-applies.
func TestApplyEnd(t *testing.T) {
	d := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		spec  string
		check func(model.StopConditions) bool
	}{
		{"until=2026-12-01", func(s model.StopConditions) bool {
			return s.Deadline != nil && s.Deadline.Equal(d) && s.ExpiresAfterSec == 0 && !s.UntilAllClosed
		}},
		{"expires=30d", func(s model.StopConditions) bool { return s.ExpiresAfterSec == 30*86400 && s.Deadline == nil }},
		{"all-closed:2d", func(s model.StopConditions) bool { return s.UntilAllClosed && s.ArchiveGraceSec == 172800 }},
		{"all-closed:2d,expires=30d", func(s model.StopConditions) bool { return s.UntilAllClosed && s.ExpiresAfterSec == 30*86400 }},
	} {
		w := &model.Critic{Stop: model.DefaultStop()}
		if err := applyEnd(w, c.spec); err != nil || !c.check(w.Stop) {
			t.Errorf("%s: err %v stop %+v", c.spec, err, w.Stop)
		}
	}
	for _, bad := range []string{"", "none", "none,expires=30d", "expires=30d,none", "expires=30m", "until=tomorrow", "all-closed:x", "forever"} {
		w := &model.Critic{Stop: model.DefaultStop()}
		if err := applyEnd(w, bad); err == nil || w.Stop != model.DefaultStop() {
			t.Errorf("%q: accepted or half-applied: %v %+v", bad, err, w.Stop)
		}
	}
}

// with-role: the critic's alert role by default, or the given role; alone only (a standing critic has no calendar
// end); a critic without an alert role must name one. RED if with-role combines with another end or loses its role.
func TestApplyEndWithRole(t *testing.T) {
	alerted := &model.Critic{Name: "author-chiqun", Author: &model.AuthorRef{AlertRole: "@fd3ede9b"}, Stop: model.DefaultStop()}
	if err := applyEnd(alerted, "with-role"); err != nil || alerted.Stop.WithRole != "@fd3ede9b" || alerted.Stop.ExpiresAfterSec != 0 || alerted.Stop.UntilAllClosed {
		t.Fatalf("default role: %v %+v", err, alerted.Stop)
	}
	seat := &model.Critic{Name: "sec-chiqun-dm", Stop: model.DefaultStop()}
	if err := applyEnd(seat, "with-role=@96b0034f"); err != nil || seat.Stop.WithRole != "@96b0034f" {
		t.Fatalf("given role: %v %+v", err, seat.Stop)
	}
	for _, bad := range []string{"with-role", "with-role,expires=30d", "expires=30d,with-role=@x", "with-role=nope"} {
		w := &model.Critic{Name: "sec-x", Stop: model.DefaultStop()}
		if err := applyEnd(w, bad); err == nil || w.Stop != model.DefaultStop() {
			t.Errorf("%q: accepted or half-applied: %v %+v", bad, err, w.Stop)
		}
	}
}

// ebac ls reflects an `ebac set --end` at once: it reads the forms, never a published index that lags the sets and
// rounds (the thinker 11-19: keel-review-keelpost showed END none while its form held with_role). RED if ls goes back
// to the index.
func TestLiveLsShowsASetEndAtOnce(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.FormNew(ctx, "ebac-livetest-ls")
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, f, id)
	name := "livetest-ls-" + strings.TrimPrefix(id, "@")[:8]
	w := &model.Critic{Name: name, Mode: model.ModeTeams, Source: "teams-chat", FormID: id, EverySec: 60, CreatedAt: time.Now().UTC(),
		Chat: &model.ChatRef{ID: "48:notes", Topic: "t"}}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	ls := func() string {
		r, wp, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = wp
		err := cmdLs(ctx, globals{}, []string{"-j"})
		wp.Close()
		os.Stdout = old
		b, _ := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Critics []struct{ Critic, End string } `json:"critics"`
		}
		_ = json.Unmarshal(b, &out)
		for _, c := range out.Critics {
			if c.Critic == name {
				return c.End
			}
		}
		t.Fatalf("%s not in ls:\n%s", name, b)
		return ""
	}
	before := ls()
	if err := cmdSet(ctx, globals{}, []string{"--critic", name, "--end", "until=2027-01-02"}); err != nil {
		t.Fatal(err)
	}
	if after := ls(); after != "until 2027-01-02" || after == before {
		t.Fatalf("ls END %q after set (before %q), want %q", after, before, "until 2027-01-02")
	}
}
