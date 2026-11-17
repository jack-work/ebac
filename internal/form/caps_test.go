package form

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// The head is what the board and a studier read of a PR: identity, state and
// counts, no ids or digests.
func TestPRHeadsCountAndWaitOn(t *testing.T) {
	c := func(id, by string, bot bool, state string) *model.Comment {
		return &model.Comment{ID: id, Author: by, IsBot: bot, State: state, Digest: "dddddddddddddddd"}
	}
	pr := &model.PRState{Key: "acme/web.site#7", Owner: "acme", Repo: "web.site", Number: 7, URL: "u", Title: "t",
		Author: "jack", State: "OPEN", HeadSHA: "abc", ReviewDecision: "REVIEW_REQUIRED", Complete: true,
		Threads: map[string]*model.Thread{
			"T1": {ID: "T1", Comments: []*model.Comment{c("a", "amy", false, ""), c("b", "jack", false, "")}},        // jack answered: waits on amy
			"T2": {ID: "T2", Comments: []*model.Comment{c("c", "bob", false, ""), c("d", "jack", false, "PENDING")}}, // pending reply: waits on jack
			"T3": {ID: "T3", IsResolved: true, Comments: []*model.Comment{c("e", "bot", true, "")}},
			"T4": {ID: "T4", Comments: []*model.Comment{c("f", "jack", false, "PENDING")}}, // nothing published
		},
		Reviews:  map[string]*model.Review{"R1": {ID: "R1", Author: "amy", State: "APPROVED"}},
		Comments: map[string]*model.Comment{"I1": c("I1", "bot", true, ""), "I2": c("I2", "amy", false, "")},
	}
	h := prsHeads(&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}})
	got := h[PrFormKey(pr.Key)]
	if got == nil {
		t.Fatalf("heads keyed %v", h)
	}
	want := prCounts{Threads: 4, Unresolved: 3, Comments: 8, Bots: 2, Reviews: 1}
	if got.N != want {
		t.Fatalf("counts %+v, want %+v", got.N, want)
	}
	if fmt.Sprint(got.WaitingOn) != "map[amy:1 jack:1]" {
		t.Fatalf("waiting_on %v", got.WaitingOn)
	}
	raw := string(model.MustJSON(h))
	for _, leak := range []string{`"T1"`, `"dddd`, `"c":`, `"threads":{`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("%s on the head: %s", leak, raw)
		}
	}
	// A closed PR waits on nobody.
	pr.State = "MERGED"
	if h := prsHeads(&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}}); h[PrFormKey(pr.Key)].WaitingOn != nil {
		t.Fatalf("a merged PR waits on %v", h[PrFormKey(pr.Key)].WaitingOn)
	}
}

func longItems(n int, at func(i int) string) map[string]InboxItem {
	out := map[string]InboxItem{}
	for i := 0; i < n; i++ {
		body := strings.Repeat("é", 1000) // 2000 B, the old cut
		switch i % 3 {
		case 0:
			out[fmt.Sprintf("IC%02d", i)] = InboxItem{PR: "a/b#1", Kind: "pr_comment_new", By: "amy", At: at(i), URL: fmt.Sprintf("https://x/c/%d", i), Body: body}
		case 1:
			out[fmt.Sprintf("keel-%02d", i)] = InboxItem{Kind: "keel_new", By: "bob", At: at(i), Fetch: "keel r, thread t: GET ...", Body: body}
		default:
			out[fmt.Sprintf("teams-%02d", i)] = InboxItem{Kind: "teams_new", By: "carl", At: at(i), Body: body}
		}
	}
	return out
}

func stamp(i int) string { return time.Unix(int64(1000+i), 0).UTC().Format(time.RFC3339) }

// The caps hold for any mix, every cut item still names its full text, the
// newest are kept, and an evicted item leaves nothing behind.
func TestInboxCapsHold(t *testing.T) {
	box := mergeInbox(nil, longItems(40, stamp))
	raw := model.MustJSON(box)
	if len(box) > inboxItems || len(raw) > inboxBytes {
		t.Fatalf("inbox %d items, %d B; caps %d items, %d B", len(box), len(raw), inboxItems, inboxBytes)
	}
	for id, it := range box {
		if len(it.Body) > model.InboxItemBytes+len("…") || !it.Cut || !json.Valid(model.MustJSON(it.Body)) {
			t.Fatalf("%s: %d B, cut %v", id, len(it.Body), it.Cut)
		}
		// github items keep their url, keel and release items their fetch;
		// a teams item's full text is the critic's inbox_fetch with its id.
		if it.URL == "" && it.Fetch == "" && !strings.HasPrefix(id, "teams-") {
			t.Fatalf("%s is cut and names no full text: %+v", id, it)
		}
	}
	for _, id := range []string{"IC39", "keel-37", "teams-38"} {
		if _, ok := box[id]; !ok {
			t.Fatalf("newest item %s evicted", id)
		}
	}
	for _, id := range []string{"IC00", "keel-01", "teams-02"} {
		if strings.Contains(string(raw), `"`+id+`"`) {
			t.Fatalf("evicted %s left a trace: %s", id, raw)
		}
	}
	// The item cap binds when items are small (Teams one-liners).
	small := map[string]InboxItem{}
	for i := 0; i < 40; i++ {
		small[fmt.Sprintf("teams-%02d", i)] = InboxItem{Kind: "teams_new", By: "carl", At: stamp(i), Body: "ok"}
	}
	if got := mergeInbox(nil, small); len(got) != inboxItems {
		t.Fatalf("small items: %d kept, want %d", len(got), inboxItems)
	}
}

// A form written under the old caps (24 KB, 2000 B items) is cut on its
// first round with nothing fresh, the change touches only body and cut (the
// item guard passes it whole), and the next round writes nothing.
func TestInboxOldFormMigratesOnceAndSettles(t *testing.T) {
	old := map[string]InboxItem{}
	for id, it := range longItems(12, stamp) {
		it.Body, it.Cut = model.CutBody(it.Body, 2000)
		old[id] = it
	}
	cur := model.MustJSON(old)
	if len(cur) <= inboxBytes {
		t.Fatalf("setup: the old inbox is %d B, not over the new cap", len(cur))
	}
	first := mergeInbox(cur, map[string]InboxItem{})
	next := model.MustJSON(first)
	// Cut, not evicted: all twelve fit once each is cut.
	if len(next) > inboxBytes || len(first) != len(old) {
		t.Fatalf("migrated inbox %d of %d items, %d B", len(first), len(old), len(next))
	}
	for id, it := range first {
		if len(it.Body) > model.InboxItemBytes+len("…") || !it.Cut {
			t.Fatalf("%s kept %d B (cut %v)", id, len(it.Body), it.Cut)
		}
	}
	set := map[string]json.RawMessage{"inbox": next}
	if n, paths := guardItems(map[string]json.RawMessage{"inbox": cur}, set); n != 0 {
		t.Fatalf("the item guard reverted %d field(s) of the cut: %v", n, paths)
	}
	for id, it := range first {
		was := old[id]
		if it.At != was.At || it.URL != was.URL || it.Fetch != was.Fetch || it.Kind != was.Kind || it.By != was.By {
			t.Fatalf("%s: the cut touched more than body and cut: %+v -> %+v", id, was, it)
		}
	}
	if again := model.MustJSON(mergeInbox(next, map[string]InboxItem{})); string(again) != string(next) {
		t.Fatal("a second round re-cut an already cut inbox")
	}
}
