package main

import (
	"context"
	"github.com/gluck/ebac/internal/livetest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
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
	if err := st.SaveCritic(ctx, w); err != nil {
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

// ebac set --every: the interval lands in every_sec and a running critic's timer is re-armed at it; a stopped one's
// stays off; too short, under a keel feed's floor, or above the collect ping is refused and changes nothing.
func TestSetEvery(t *testing.T) {
	w := &model.Critic{Name: "author-jack", EverySec: 120}
	if err := setEvery(w, 5*time.Minute, 0); err != nil || w.EverySec != 300 {
		t.Fatalf("5m: err %v every_sec %d", err, w.EverySec)
	}
	var armed []string
	enable := func(name string, d time.Duration) error { armed = append(armed, name+" "+d.String()); return nil }
	if ok, err := rearm(w, enable); !ok || err != nil || strings.Join(armed, ",") != "author-jack 5m0s" {
		t.Fatalf("re-arm: %v %v %q", ok, err, armed)
	}
	w.Runtime.Stopped, armed = true, nil
	if ok, _ := rearm(w, enable); ok || armed != nil {
		t.Fatalf("a stopped critic's timer was armed: %q", armed)
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

// ebac rm of a critic whose form is gone (removed, or its log lost: "xwal: unknown trunk", 11-19) still drops its
// timer and drop-ins by name, and refuses a name that is not a critic name.
func TestRemoveLeftovers(t *testing.T) {
	var did []string
	disable := func(n string) error { did = append(did, "timer "+n); return nil }
	dropIns := func(n string) error { did = append(did, "drop-ins "+n); return nil }
	if err := removeLeftovers("livetest-set-17e13c59", disable, dropIns, form.ErrNoCritic); err != nil {
		t.Fatal(err)
	}
	if strings.Join(did, ",") != "timer livetest-set-17e13c59,drop-ins livetest-set-17e13c59" {
		t.Fatalf("did %q", did)
	}
	did = nil
	if err := removeLeftovers("../x", disable, dropIns, form.ErrNoCritic); err == nil || did != nil {
		t.Fatalf("a bad name was acted on: %v %q", err, did)
	}
}
