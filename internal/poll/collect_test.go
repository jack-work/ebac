package poll

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

func wakeEv(id string) model.Event {
	return model.Event{Kind: "teams_new", Tier: model.TierWake, PR: "chat", Comment: id, Author: "Akshay"}
}

// W7 item 2 proof, at the delivery seam: messages arriving between pings are held, not sent; the next ping tick
// sends exactly one one-line ping with the count; a tick with nothing new sends nothing. Control: ordinary delivery
// (no ping cadence) sends on every round with news.
func TestCollectModePingsOncePerTick(t *testing.T) {
	ctx := context.Background()
	var sent []string
	p := &Poller{Out: io.Discard,
		Send:   func(_ context.Context, _, msg string) error { sent = append(sent, msg); return nil },
		Holder: func(context.Context, string) bool { return true }}
	w := &model.Critic{Name: "teams-boss", FormID: "@f", Source: "teams-chat", PingEverySec: 1800}

	add := func(ids ...string) {
		for _, id := range ids {
			if len(w.Runtime.PendingEvents) == 0 {
				w.Runtime.PendingSince = time.Now().UTC()
			}
			w.Runtime.PendingEvents = MergeEvents(w.Runtime.PendingEvents, []model.Event{wakeEv(id)})
		}
	}
	deliver := func() {
		t.Helper()
		if err := p.Deliver(ctx, w, nil); err != nil {
			t.Fatal(err)
		}
	}

	add("1")
	deliver() // the first news pings at once: nothing was pinged yet
	if len(sent) != 1 || !strings.Contains(sent[0], "1 new since") {
		t.Fatalf("first ping: %q", sent)
	}
	// Three messages in two minutes, each round inside the ping window: held, not sent.
	for _, id := range []string{"2", "3", "4"} {
		add(id)
		deliver()
	}
	if len(sent) != 1 || len(w.Runtime.PendingEvents) != 3 {
		t.Fatalf("collect sent %d pings (want 1), pending %d (want 3)", len(sent), len(w.Runtime.PendingEvents))
	}
	// The ping tick: exactly one ping, one line, counting all three.
	w.Runtime.LastPing = time.Now().Add(-31 * time.Minute)
	deliver()
	if len(sent) != 2 || !strings.Contains(sent[1], "3 new since") || strings.Contains(sent[1], "\n") || len(w.Runtime.PendingEvents) != 0 {
		t.Fatalf("tick: sent %q, pending %d", sent, len(w.Runtime.PendingEvents))
	}
	// A later tick with nothing new pings nothing.
	w.Runtime.LastPing = time.Now().Add(-31 * time.Minute)
	deliver()
	if len(sent) != 2 {
		t.Fatalf("an empty tick pinged: %q", sent[len(sent)-1])
	}

	// Control: no ping cadence, every round with news is sent at once (the plugin's brief, not a ping line).
	sent = nil
	p.Plugins = plugin.InProc{"tomb": func(context.Context, plugin.Request) (*plugin.Reply, error) {
		return &plugin.Reply{Brief: "full brief"}, nil
	}}
	w = &model.Critic{Name: "teams-x", FormID: "@g", Source: "teams-chat"}
	for _, id := range []string{"a", "b"} {
		add(id)
		deliver()
	}
	if len(sent) != 2 || sent[0] != "full brief" {
		t.Fatalf("control: ordinary delivery sent %q; want two full briefs", sent)
	}
}

func TestPingHeld(t *testing.T) {
	now := time.Date(2026, 11, 9, 23, 0, 0, 0, time.UTC)
	w := &model.Critic{PingEverySec: 1800}
	if PingHeld(w, now) {
		t.Fatal("never pinged: must not hold")
	}
	w.Runtime.LastPing = now.Add(-10 * time.Minute)
	if !PingHeld(w, now) {
		t.Fatal("pinged 10m ago with a 30m cadence: must hold")
	}
	w.Runtime.LastPing = now.Add(-31 * time.Minute)
	if PingHeld(w, now) {
		t.Fatal("pinged 31m ago: the tick is due")
	}
	w.PingEverySec, w.Runtime.LastPing = 0, now
	if PingHeld(w, now) {
		t.Fatal("ordinary delivery never holds")
	}
}

// A release watch may set its releases key and nothing else.
func TestReleasesSourceKeys(t *testing.T) {
	ok := &plugin.Reply{Set: map[string]json.RawMessage{"releases": json.RawMessage(`[]`)}}
	if err := SourceKeysOnly(model.SourceReleases, ok); err != nil {
		t.Fatalf("releases refused: %v", err)
	}
	bad := &plugin.Reply{Set: map[string]json.RawMessage{"authored": json.RawMessage(`{}`)}}
	if err := SourceKeysOnly(model.SourceReleases, bad); err == nil || !strings.Contains(err.Error(), "authored") {
		t.Fatalf("control: a release watch writing authored must be refused, naming it: %v", err)
	}
}
