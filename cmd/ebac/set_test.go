package main

import (
	"context"
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
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
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
