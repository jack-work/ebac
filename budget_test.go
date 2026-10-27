package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func TestInboxKeepsOthersCutsAndEvictsOldest(t *testing.T) {
	_, snap := outboxCritic()
	pr := snap.PRs["acme/widget#1"]
	pr.Comments["IC1"] = &model.Comment{ID: "IC1", Author: "amy", URL: "u1", Body: strings.Repeat("é", 1500)}
	pr.Comments["IC2"] = &model.Comment{ID: "IC2", Author: "jack", URL: "u2", Body: "mine"}
	ev := []model.Event{{Kind: model.EvPRCommentNew, PR: pr.Key, Comment: "IC1"}, {Kind: model.EvPRCommentNew, PR: pr.Key, Comment: "IC2"}}
	got := inboxItems(snap, ev, "jack", time.Unix(0, 0))
	if _, mine := got["IC2"]; mine || len(got) != 1 {
		t.Fatalf("inbox %v; want only amy's comment", got)
	}
	it := got["IC1"]
	if !it.Cut || len(it.Body) > inboxItemBytes+len("…") || !strings.HasSuffix(it.Body, "…") || !json.Valid(model.MustJSON(it.Body)) {
		t.Fatalf("cut: %v, %d bytes", it.Cut, len(it.Body))
	}

	fresh := map[string]InboxItem{}
	for i := 0; i < 30; i++ {
		id := string(rune('A'+i%26)) + string(rune('a'+i/26))
		fresh[id] = InboxItem{PR: "p", By: "amy", At: time.Unix(int64(i), 0).UTC().Format(time.RFC3339), Body: strings.Repeat("x", 1900)}
	}
	box := mergeInbox(nil, fresh)
	if n := len(model.MustJSON(box)); n > inboxBytes {
		t.Fatalf("inbox is %d B, over %d", n, inboxBytes)
	}
	if _, ok := box["Aa"]; ok {
		t.Fatal("the oldest item survived eviction")
	}
	if _, ok := box["Db"]; !ok {
		t.Fatal("the newest item was evicted")
	}
}

func TestPrunePostsKeepsUnfinishedAndRecent(t *testing.T) {
	now := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	posts := map[string]json.RawMessage{
		"open":   model.MustJSON(map[string]any{"pr": "a/b#1", "body": "x"}),
		"recent": model.MustJSON(map[string]any{"pr": "a/b#1", "body": "x", "sent": map[string]string{"url": "u"}, "done_at": now.Add(-10 * time.Minute).Format(time.RFC3339)}),
		"old":    model.MustJSON(map[string]any{"pr": "a/b#1", "body": "x", "sent": map[string]string{"url": "u"}, "done_at": now.Add(-2 * time.Hour).Format(time.RFC3339)}),
		"legacy": model.MustJSON(map[string]any{"pr": "a/b#1", "body": "x", "refused": "no"}),
	}
	if !prunePosts(posts, now) {
		t.Fatal("nothing pruned")
	}
	for id, want := range map[string]bool{"open": true, "recent": true, "old": false, "legacy": false} {
		if _, ok := posts[id]; ok != want {
			t.Errorf("%s present=%v, want %v", id, ok, want)
		}
	}
}

// Cheroot's control for the guard: a write leaving the state at 191 KB lands,
// one at 193 KB is refused, loudly, and the form keeps its last good state.
func TestLiveBudgetGuardControl(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, _ := NewStore(f)
	id, err := f.FormNew(ctx, "ebac-livetest-budget")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	fill := func(total int) error {
		return st.commit(ctx, id, func(cur map[string]json.RawMessage, _ ebacState) (map[string]json.RawMessage, []string) {
			base := 0
			for k, v := range cur {
				if k != "filler" {
					base += len(k) + len(v) + 4
				}
			}
			n := total - base - len(`"filler":""`) - 2
			return map[string]json.RawMessage{"filler": model.MustJSON(strings.Repeat("x", n))}, nil
		})
	}
	size := func() int {
		fr, err := st.readForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(fr.Snap)
		return len(b)
	}
	if err := fill(191 * 1024); err != nil {
		t.Fatalf("191 KB write refused: %v", err)
	}
	landed := size()
	if landed < 190*1024 || landed > FormStateBudget {
		t.Fatalf("positive control: state is %d B after the 191 KB write", landed)
	}
	err = fill(193 * 1024)
	if err == nil || !strings.Contains(err.Error(), "over the 196608 B budget") {
		t.Fatalf("193 KB write: %v; want a refusal naming the budget", err)
	}
	if got := size(); got != landed {
		t.Fatalf("a refused write changed the form: %d -> %d B", landed, got)
	}
	t.Logf("191 KB: landed at %d B; 193 KB: %v", landed, err)
}
