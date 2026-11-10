package teams

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

// The 2026-11-10 wipe: after a TLS outage the blind-recovery round of 4 Teams critics wrote critic: {} and they
// vanished from ebac ls. Replays blind then recover over the real merge path, a FRESH store per round (each round is
// its own process in production), and asserts the critic key is byte-identical afterwards.
func TestLiveBlindRecoveryKeepsTheCritic(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-wipe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	root := t.TempDir()
	store := func() *form.Store {
		st, err := form.NewStore(f)
		if err != nil {
			t.Fatal(err)
		}
		st.Root = root
		return st
	}
	w := &model.Critic{Name: "livetest-wipe-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, EverySec: 60, Chat: testChat()}
	if err := store().SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := store().SeedTrigger(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) error {
		p := &poll.Poller{Store: store(), Fig: f, Out: &out, Plugins: plugin.InProc{"tomb": Plugin{Tomb: fakeTomb(t, scenario)}.Handle}}
		return p.Poll(ctx, w.Name)
	}
	critic := func() string {
		keys, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(keys["critic"])
	}
	if err := round("ok"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// The victim forms' top-level key set (teams-akshay-chiqun-minister @d7664712): the tree shape the old form
	// library mis-deleted depends on it. target-aria names no live aria, so nothing is ever woken.
	for k, v := range map[string]any{"ack_at": "2026-11-10T18:00:00Z", "ack_note": "n", "ack_round": 1, "inbox": map[string]any{},
		"target-aria": "0000000000000000deadbeef00000000"} {
		if err := f.SetJSON(ctx, id, k, v); err != nil {
			t.Fatal(err)
		}
	}
	before := critic()
	orig := teamsBlindAfter
	teamsBlindAfter = 0
	t.Cleanup(func() { teamsBlindAfter = orig })
	for i := 0; i < 3; i++ {
		_ = round("down")
	}
	if err := round("ok"); err != nil {
		t.Fatalf("recovery: %v\n%s", err, out.String())
	}
	if after := critic(); after != before {
		t.Fatalf("critic changed across blind + recovery:\n before %s\n after  %s\n%s", before, after, out.String())
	}
	keys, _, _ := f.FormRaw(ctx, id)
	if _, blind := keys["blind"]; blind {
		t.Fatalf("recovery left blind on the form\n%s", out.String())
	}
	if _, ok := keys["trigger"]; !ok {
		t.Fatal("trigger lost")
	}
	if _, err := store().LoadCritic(ctx, w.Name); err != nil {
		t.Fatalf("critic unreadable after recovery: %v", err)
	}
}
