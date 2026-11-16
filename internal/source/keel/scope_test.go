package keel

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

func keysOf(kv ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = model.MustJSON(kv[i+1])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// threadsSet is the threads key a reply sets, nil when it sets none.
func threadsSet(t *testing.T, rep *plugin.Reply) map[string]map[string]any {
	t.Helper()
	raw, ok := rep.Set["threads"]
	if !ok {
		return nil
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func eachMode(t *testing.T, f func(t *testing.T, feed bool)) {
	for _, m := range []struct {
		name string
		feed bool
	}{{"threads", false}, {"feed", true}} {
		t.Run(m.name, func(t *testing.T) { f(t, m.feed) })
	}
}

func TestCompileScope(t *testing.T) {
	for _, c := range []struct {
		spec model.KeelScope
		ok   bool
	}{
		{model.KeelScope{}, true},
		{model.KeelScope{Threads: "all", Inbox: "all"}, true},
		{model.KeelScope{Threads: "changed", Inbox: "heads"}, true},
		{model.KeelScope{Inbox: "last:3"}, true},
		{model.KeelScope{Mirror: []string{"inbox", "threads.resolved", "threads.n"}}, true},
		{model.KeelScope{Mirror: []string{"inbox.body", "inbox.fetch"}}, true},
		{model.KeelScope{Threads: "new"}, false},
		{model.KeelScope{Inbox: "last:0"}, false},
		{model.KeelScope{Inbox: "last:x"}, false},
		{model.KeelScope{Inbox: "first"}, false},
		{model.KeelScope{Mirror: []string{"blind"}}, false},
		{model.KeelScope{Mirror: []string{"inbox.kind"}}, false},
		{model.KeelScope{Mirror: []string{"threads.body"}}, false},
	} {
		if err := CheckScope(c.spec); (err == nil) != c.ok {
			t.Errorf("%+v: err %v, want ok %v", c.spec, err, c.ok)
		}
	}
}

// Every scope cuts only what is written: events, tiers and the seen state are
// those of the default round.
func TestAScopeNeverChangesWhatWakes(t *testing.T) {
	scopes := []map[string]json.RawMessage{
		keysOf(model.KeelThreadsScopeKey, "changed"),
		keysOf(model.KeelInboxScopeKey, "heads"),
		keysOf(model.KeelInboxScopeKey, "last:1"),
		keysOf(model.KeelMirrorKey, "threads"),
		keysOf(model.KeelMirrorKey, "inbox.body"),
		keysOf(model.KeelThreadsScopeKey, "changed", model.KeelInboxScopeKey, "heads", model.KeelMirrorKey, "threads.n"),
	}
	eachMode(t, func(t *testing.T, feed bool) {
		want := runScripted(t, feed, nil)
		for _, keys := range scopes {
			got := runScripted(t, feed, keys)
			for i := range want {
				we, ge := plugin.Unwire(want[i].Events), plugin.Unwire(got[i].Events)
				if !reflect.DeepEqual(we, ge) {
					t.Fatalf("%s round %d: events %+v, want %+v", keys, i+1, ge, we)
				}
				ws, gs := want[i].Critic.Runtime.Keel, got[i].Critic.Runtime.Keel
				if !reflect.DeepEqual(ws.Seen, gs.Seen) || ws.Cursor != gs.Cursor || !reflect.DeepEqual(ws.Resolved, gs.Resolved) {
					t.Fatalf("%s round %d: state %+v, want %+v", keys, i+1, gs, ws)
				}
			}
		}
	})
}

func TestThreadsChangedListsOnlyNewsAndKeepsItThroughQuietRounds(t *testing.T) {
	eachMode(t, func(t *testing.T, feed bool) {
		reps := runScripted(t, feed, keysOf(model.KeelThreadsScopeKey, "changed"))
		if got := threadsSet(t, reps[0]); got != nil {
			t.Fatalf("baseline wrote threads %v; it has no news", got)
		}
		if got := sortedKeys(threadsSet(t, reps[1])); strings.Join(got, ",") != "t1,t2,t3,t4" {
			t.Fatalf("round 2 threads %v", got)
		}
		if got := threadsSet(t, reps[2]); got != nil {
			t.Fatalf("a quiet round rewrote threads: %v", got)
		}
		got := threadsSet(t, reps[3])
		if strings.Join(sortedKeys(got), ",") != "t1" || got["t1"]["n"] != float64(2) {
			t.Fatalf("round 4 (an edit in t1) threads %v", got)
		}
	})
}

func inboxLinks(rep *plugin.Reply) map[string][]string {
	out := map[string][]string{}
	for _, e := range rep.Events {
		if e.Comment != "" {
			out[e.Comment] = e.Inbox
		}
	}
	return out
}

func TestInboxHeadsKeepsFirstCommentsOnly(t *testing.T) {
	eachMode(t, func(t *testing.T, feed bool) {
		reps := runScripted(t, feed, keysOf(model.KeelInboxScopeKey, "heads"))
		if got := sortedKeys(reps[1].Inbox); strings.Join(got, ",") != "keel-h1" {
			t.Fatalf("round 2 inbox %v", got)
		}
		links := inboxLinks(reps[1])
		if len(links) != 4 || len(links["h1"]) != 1 || links["c2"] != nil || links["h2"] != nil || links["o2"] != nil {
			t.Fatalf("round 2 links %v", links)
		}
		if len(reps[3].Inbox) != 0 {
			t.Fatalf("an edit of a reply reached the inbox: %v", sortedKeys(reps[3].Inbox))
		}
	})
}

func TestInboxLastNKeepsTheNewestPerThread(t *testing.T) {
	eachMode(t, func(t *testing.T, feed bool) {
		reps := runScripted(t, feed, keysOf(model.KeelInboxScopeKey, "last:1"))
		if got := sortedKeys(reps[1].Inbox); strings.Join(got, ",") != "keel-c2,keel-h2,keel-o2" {
			t.Fatalf("round 2 inbox %v", got)
		}
		reps = runScripted(t, feed, keysOf(model.KeelInboxScopeKey, "last:2"))
		if got := sortedKeys(reps[1].Inbox); strings.Join(got, ",") != "keel-c2,keel-h1,keel-h2,keel-o2" {
			t.Fatalf("last:2 round 2 inbox %v", got)
		}
	})
}

func TestMirrorFields(t *testing.T) {
	eachMode(t, func(t *testing.T, feed bool) {
		reps := runScripted(t, feed, keysOf(model.KeelMirrorKey, "inbox.body, threads.resolved,threads.n"))
		it := reps[1].Inbox["keel-c2"]
		if it.Body != "yes, ship it" || it.Kind != string(EvKeelNew) || it.By != boss || it.At == "" ||
			it.Thread != "" || it.Path != "" || it.Line != 0 || it.Quote != "" || it.Fetch != "" || it.Time != "" {
			t.Fatalf("item %+v", it)
		}
		th := threadsSet(t, reps[1])
		if !reflect.DeepEqual(th["t3"], map[string]any{"resolved": true, "n": float64(1)}) || !reflect.DeepEqual(th["t1"], map[string]any{"n": float64(2)}) {
			t.Fatalf("threads %v", th)
		}
	})
}

func TestMirrorDropsUnnamedKeys(t *testing.T) {
	eachMode(t, func(t *testing.T, feed bool) {
		reps := runScripted(t, feed, keysOf(model.KeelMirrorKey, "inbox"))
		for i, rep := range reps {
			if _, ok := rep.Set["threads"]; ok || !contains(rep.Unset, "threads") {
				t.Fatalf("round %d: set %v unset %v", i+1, sortedKeys(rep.Set), rep.Unset)
			}
		}
		if len(reps[1].Inbox) != 4 {
			t.Fatalf("inbox %v", sortedKeys(reps[1].Inbox))
		}
		reps = runScripted(t, feed, keysOf(model.KeelMirrorKey, "threads"))
		if reps[1].Inbox != nil || len(inboxLinks(reps[1])["c2"]) != 0 || threadsSet(t, reps[1]) == nil {
			t.Fatalf("inbox %v links %v", reps[1].Inbox, inboxLinks(reps[1]))
		}
	})
}

// A bad key keeps the last good scope and says so in scope_error, once; the
// fix clears it.
func TestABadScopeKeepsTheLastGoodOne(t *testing.T) {
	keys := []map[string]json.RawMessage{
		keysOf(model.KeelThreadsScopeKey, "changed"),
		keysOf(model.KeelThreadsScopeKey, "recent"),
		keysOf(model.KeelThreadsScopeKey, "recent"),
		nil,
	}
	reps := runRounds(t, false, func(i int) map[string]json.RawMessage { return keys[i] })
	var e0 string
	if _, ok := reps[0].Set[model.KeelScopeErr]; ok {
		t.Fatal("a good scope wrote scope_error")
	}
	if err := json.Unmarshal(reps[1].Set[model.KeelScopeErr], &e0); err != nil || !strings.Contains(e0, "the last good scope is in force") || !strings.Contains(e0, "recent") {
		t.Fatalf("round 2 scope_error %q (%v)", e0, err)
	}
	if got := sortedKeys(threadsSet(t, reps[1])); strings.Join(got, ",") != "t1,t2,t3,t4" {
		t.Fatalf("round 2 threads %v", got) // changed still in force: all four had news
	}
	if _, ok := reps[2].Set[model.KeelScopeErr]; ok || threadsSet(t, reps[2]) != nil {
		t.Fatalf("round 3 rewrote scope_error or threads: %v", sortedKeys(reps[2].Set))
	}
	if !contains(reps[3].Unset, model.KeelScopeErr) || reps[3].Critic.Runtime.Keel.Scope != nil || len(threadsSet(t, reps[3])) != 4 {
		t.Fatalf("round 4: unset %v scope %+v", reps[3].Unset, reps[3].Critic.Runtime.Keel.Scope)
	}
	// A broken first value with no last good one writes everything.
	st := &model.KeelState{}
	sc, msg := resolveScope(st, keysOf(model.KeelInboxScopeKey, "all-of-it"))
	if sc.partial() || !strings.Contains(msg, "no scope (everything written) is in force") {
		t.Fatalf("scope %+v msg %q", sc, msg)
	}
}

func TestBriefNamesTheFullViewUnderAPartialScope(t *testing.T) {
	w := &model.Critic{Name: "notes", FormID: "@f", Keel: ref(), Runtime: model.Runtime{Round: 3, Keel: &model.KeelState{}}}
	evs := []model.Event{{Kind: EvKeelNew, Tier: model.TierWake, Author: boss, Path: "doc/a.md", ThreadID: "t1", Detail: "yes"}}
	if got := keelBrief(w, evs); !strings.Contains(got, "key threads indexes every thread. When finished") || strings.Contains(got, "Partial") {
		t.Fatalf("default brief %q", got)
	}
	w.Runtime.Keel.Scope = &model.KeelScope{Threads: "changed", Inbox: "last:2", Mirror: []string{"inbox", "threads.n"}}
	got := keelBrief(w, evs)
	for _, want := range []string{"Partial scope:", "slept through a round", "2 newest comments", "the form mirrors threads (n) and inbox",
		"The full view: keel jack-work/notes: GET /api/r/jack-work-notes/threads.", "ebac ack --critic notes --round 3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief lacks %q:\n%s", want, got)
		}
	}
}
