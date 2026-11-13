package form

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	fform "github.com/jack-work/figaro/api/form"
)

// The form-churn order (2026-11-13): a Teams inbox item is written once.
// Each test pairs the refusal with a positive control, so a no-op merge
// cannot pass for an immutable one.

func boxOf(t *testing.T, items map[string]InboxItem) json.RawMessage {
	t.Helper()
	return model.MustJSON(items)
}

func held() map[string]InboxItem {
	return map[string]InboxItem{
		"teams-1": {Kind: "teams_new", By: "Akshay", At: "2026-11-13T01:00:00Z", Time: "t1", Body: "first", FromID: "8:orgid:a"},
	}
}

// (a) an old entry's at is never re-touched, by an edit or by a resend.
func TestInboxOldEntryAtNeverRetouched(t *testing.T) {
	cur := boxOf(t, held())
	got := mergeInbox(cur, map[string]InboxItem{
		"teams-1": {Kind: "teams_edited", By: "Akshay", At: "2026-11-13T02:00:00Z", Body: "first, edited"},
	})
	if got["teams-1"].At != "2026-11-13T01:00:00Z" || got["teams-1"].Kind != "teams_new" || got["teams-1"].FromID != "8:orgid:a" {
		t.Fatalf("edit re-touched the entry: %+v", got["teams-1"])
	}
	if got["teams-1"].Body != "first, edited" || got["teams-1"].EditedAt != "2026-11-13T02:00:00Z" {
		t.Fatalf("control: the edit itself did not land: %+v", got["teams-1"])
	}
	// control: a new item does get its own at.
	got = mergeInbox(cur, map[string]InboxItem{"teams-2": {Kind: "teams_new", By: "B", At: "2026-11-13T02:00:00Z", Body: "x"}})
	if got["teams-2"].At != "2026-11-13T02:00:00Z" || got["teams-1"].At != "2026-11-13T01:00:00Z" {
		t.Fatalf("new item: %+v", got)
	}
}

// (b) teams_deleted is written once: it removes the item, and a second
// delete (or a delete of an item never held) writes nothing.
func TestInboxTeamsDeletedOnce(t *testing.T) {
	cur := boxOf(t, held())
	got := mergeInbox(cur, map[string]InboxItem{"teams-1": {Kind: "teams_deleted", At: "later"}})
	if _, ok := got["teams-1"]; ok {
		t.Fatalf("control: delete did not remove the item: %+v", got)
	}
	again := mergeInbox(boxOf(t, got), map[string]InboxItem{"teams-1": {Kind: "teams_deleted", At: "later2"}, "teams-9": {Kind: "teams_deleted"}})
	if string(model.MustJSON(again)) != string(model.MustJSON(got)) {
		t.Fatalf("a second delete wrote: %s -> %s", model.MustJSON(got), model.MustJSON(again))
	}
	if string(model.MustJSON(again)) != "{}" {
		t.Fatalf("a delete wrote a tombstone: %s", model.MustJSON(again))
	}
}

// (c) a kept-current repost that changes nothing writes nothing: a re-sent
// teams_new for a held id, or an edit with the same body.
func TestInboxUnchangedRepostWritesNothing(t *testing.T) {
	cur := boxOf(t, held())
	for name, fresh := range map[string]InboxItem{
		"resend": {Kind: "teams_new", By: "Akshay", At: "2026-11-13T03:00:00Z", Body: "first"},
		"edit":   {Kind: "teams_edited", By: "Akshay", At: "2026-11-13T03:00:00Z", Body: "first"},
		"other":  {Kind: "teams_new", By: "Akshay", At: "2026-11-13T03:00:00Z", Body: "", Time: "t9"},
	} {
		got := mergeInbox(cur, map[string]InboxItem{"teams-1": fresh})
		if string(model.MustJSON(got)) != string(cur) {
			t.Fatalf("%s rewrote a held item: %s", name, model.MustJSON(got))
		}
	}
	// control: an edit that changes the body does write.
	got := mergeInbox(cur, map[string]InboxItem{"teams-1": {Kind: "teams_edited", At: "x", Body: "changed"}})
	if string(model.MustJSON(got)) == string(cur) {
		t.Fatal("control: a real edit wrote nothing")
	}
}

// (d) an unchanged write is skipped: the commit diff of an unchanged inbox
// is the identity, while a changed one is not.
func TestInboxUnchangedWriteSkipped(t *testing.T) {
	cur := boxOf(t, held())
	snap, err := nextBoard(fform.Snapshot{}, map[string]json.RawMessage{"inbox": cur}, nil)
	if err != nil {
		t.Fatal(err)
	}
	same := mergeInbox(cur, map[string]InboxItem{"teams-1": {Kind: "teams_new", At: "later", Body: "first"}})
	next, err := nextBoard(snap, map[string]json.RawMessage{"inbox": model.MustJSON(same)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := next.Diff(snap); !p.IsIdentity() {
		t.Fatalf("an unchanged inbox wrote a patch: %+v", p)
	}
	// control: one new item is a patch of that item's leaves only.
	grown := mergeInbox(cur, map[string]InboxItem{"teams-2": {Kind: "teams_new", At: "later", Body: "two"}})
	next, _ = nextBoard(snap, map[string]json.RawMessage{"inbox": model.MustJSON(grown)}, nil)
	p := next.Diff(snap)
	if p.IsIdentity() {
		t.Fatal("control: a new item wrote nothing")
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), `"teams-1"`) {
		t.Fatalf("the patch for a new item touched the held one: %s", raw)
	}
}
