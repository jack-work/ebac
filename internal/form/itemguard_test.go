package form

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	fform "github.com/jack-work/figaro/api/form"
)

func changedPaths(t *testing.T, cur, set map[string]json.RawMessage) []string {
	t.Helper()
	raw, _ := json.Marshal(cur)
	var snap fform.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	next, err := nextBoard(snap, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range next.Diff(snap).Entries() {
		out = append(out, e.Key)
	}
	sort.Strings(out)
	return out
}

// An existing item's at/url/kind cannot be re-touched; its body (a Teams edit) can; a new item and a removed item
// pass. Positive control: the same write WITHOUT the guard re-sends the old item's at, so the check can see a
// re-touch.
func TestItemGuardKeepsExistingItems(t *testing.T) {
	cur := map[string]json.RawMessage{
		"inbox": json.RawMessage(`{"1":{"kind":"teams_new","at":"2026-11-13T01:00:00Z","body":"hi","url":"u1"},` +
			`"2":{"kind":"teams_new","at":"2026-11-13T02:00:00Z","body":"gone"}}`),
		"name": json.RawMessage(`"x"`),
	}
	write := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{"inbox": json.RawMessage(
			`{"1":{"kind":"teams_new","at":"2026-11-13T05:00:00Z","body":"hi (edited)","edited_at":"2026-11-13T05:00:00Z","url":"u1-new"},` +
				`"3":{"kind":"teams_new","at":"2026-11-13T05:00:00Z","body":"new"}}`)}
	}
	control := changedPaths(t, cur, write())
	if !contains(control, "inbox.1.at") || !contains(control, "inbox.1.url") {
		t.Fatalf("control: an unguarded write does not show the re-touch (%v); the test cannot see one", control)
	}
	set := write()
	n, paths := guardItems(cur, set)
	got := changedPaths(t, cur, set)
	t.Logf("guard kept %d: %v; patch %v", n, paths, got)
	if n != 2 || contains(got, "inbox.1.at") || contains(got, "inbox.1.url") || contains(got, "inbox.1.kind") {
		t.Fatalf("guard: kept %d (%v), patch %v; want at and url of item 1 kept", n, paths, got)
	}
	for _, want := range []string{"inbox.1.body", "inbox.1.edited_at", "inbox.2", "inbox.3"} {
		if !contains(got, want) {
			t.Fatalf("the guard blocked a legal change %s: %v", want, got)
		}
	}
	// No write to the inbox key, or a first inbox: nothing to guard.
	if n, _ := guardItems(map[string]json.RawMessage{}, write()); n != 0 {
		t.Fatalf("a first inbox was guarded: %d", n)
	}
}

// contains says whether x, or a path under it, is in xs.
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x || strings.HasPrefix(v, x+".") {
			return true
		}
	}
	return false
}
