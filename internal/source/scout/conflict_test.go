package scout

import (
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// A merge into a PR's base does not bump the PR's updatedAt (gate-10 #108680, 11-12): the base-head watch rides the
// search's first page, and only a base that moved re-asks its PRs' mergeability. A turn into conflict rewrites the
// entry ("waiting: conflict" over an unmet earlier step) and pings the owner role; UNKNOWN is asked again, never a
// conflict; nothing moved costs nothing.
func TestBaseMoveFindsConflict(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: knight}, {Author: his, Role: secretary}}
	w.gh.items[201].base = "stack/x"
	w.gh.heads = map[string]string{"main": "a1aaaaaaaa", "stack/x": "b1bbbbbbbb"}
	w.pass(0) // tracks the PRs; no base known before the first search

	w.pass(2 * time.Minute) // first sighting of the bases: their PRs' mergeability is read once
	if w.st.Bases["main"] != "a1aaaaaa" || w.st.Bases["stack/x"] != "b1bbbbbb" || w.st.PRs[201].Base != "stack/x" || w.st.PRs[101].Base != "" {
		t.Fatalf("bases %v, #201 base %q, #101 base %q", w.st.Bases, w.st.PRs[201].Base, w.st.PRs[101].Base)
	}
	w.gh.light = nil
	r := w.pass(2 * time.Minute)
	if w.gh.calls != 1 || len(w.gh.light) != 0 || len(r.Pings) != 0 {
		t.Fatalf("nothing moved: %d calls, %d mergeability reads, pings %v", w.gh.calls, len(w.gh.light), r.Pings)
	}

	// main moves and #101 turns conflicting without an update: only main's PRs are asked, #101 is re-read in full.
	w.gh.heads["main"] = "a2aaaaaaaa"
	w.gh.items[101].mergeable, w.gh.items[101].mergeState = "CONFLICTING", "DIRTY"
	r = w.pass(2 * time.Minute)
	if len(w.gh.light) != 1 || !strings.Contains(w.gh.light[0], "m101:") || strings.Contains(w.gh.light[0], "m201:") {
		t.Fatalf("mergeability reads %q, bases %v, log %v, q0 %.300s", w.gh.light, w.st.Bases, r.Log, w.gh.queries[len(w.gh.queries)-1])
	}
	if e := entry(t, r, "pr:101"); e["mg"] != "CONFLICTING" || e["ms"] != "waiting: conflict" {
		t.Fatalf("#101 entry mg %v ms %v (log %v)", e["mg"], e["ms"], r.Log)
	}
	if len(r.Pings) != 1 || r.Pings[0].To != knight || !strings.Contains(r.Pings[0].Text, "#101") || !strings.Contains(r.Pings[0].Text, "CONFLICTING with main") {
		t.Fatalf("pings %v", r.Pings)
	}
	if r = w.pass(2 * time.Minute); len(r.Pings) != 0 || r.Set["pr:101"] != nil {
		t.Fatalf("a standing conflict pinged again or rewrote: %v %v", r.Pings, r.Set["pr:101"])
	}
	w.gh.items[101].updated = w.now.Add(time.Minute) // read again (a comment, a push): still conflicting, no new ping
	if r = w.pass(2 * time.Minute); len(r.Pings) != 0 {
		t.Fatalf("a standing conflict re-read pinged again: %v", r.Pings)
	}

	// The stack base moves while GitHub recomputes: UNKNOWN is no conflict and is asked again next pass, alone.
	w.gh.light = nil
	w.gh.heads["stack/x"] = "b2bbbbbbbb"
	w.gh.items[201].mergeable = "UNKNOWN"
	r = w.pass(2 * time.Minute)
	if len(r.Pings) != 0 || r.Set["pr:201"] != nil || len(w.st.Recheck) != 1 || w.st.Recheck[0] != 201 {
		t.Fatalf("unknown: pings %v, set %s, recheck %v", r.Pings, r.Set["pr:201"], w.st.Recheck)
	}
	w.gh.light = nil
	w.gh.items[201].mergeable, w.gh.items[201].mergeState = "CONFLICTING", "DIRTY"
	r = w.pass(2 * time.Minute)
	if len(w.gh.light) != 1 || !strings.Contains(w.gh.light[0], "m201:") || strings.Contains(w.gh.light[0], "m101:") {
		t.Fatalf("recheck reads %q", w.gh.light)
	}
	// Not on main: recorded on the form (quiet_pings), never pinged (Knight wake noise, 2026-11-17).
	if len(r.Pings) != 0 || len(w.st.Recheck) != 0 || !strings.Contains(string(r.Set["quiet_pings"]), "with stack/x") ||
		!strings.Contains(string(r.Set["quiet_pings"]), secretary) {
		t.Fatalf("settled conflict on the stack base: pings %v, quiet %s, recheck %v", r.Pings, r.Set["quiet_pings"], w.st.Recheck)
	}

	// Quiet again: one call, no mergeability read.
	w.gh.light = nil
	if r = w.pass(2 * time.Minute); w.gh.calls != 1 || len(w.gh.light) != 0 || len(r.Pings) != 0 {
		t.Fatalf("quiet after: %d calls, %d reads", w.gh.calls, len(w.gh.light))
	}
}

// The mergeability aliases ask two scalars a PR and ride the re-read's query (alone when nothing is re-read); the refs
// alias rides a root query and is absent without bases.
func TestBaseWatchQueries(t *testing.T) {
	q := withMerge(batchQuery("bic/aether", nil, nil, false), []int{1, 2})
	if q != `{repository(owner:"bic", name:"aether"){m1: pullRequest(number:1){number mergeable mergeStateStatus} m2: pullRequest(number:2){number mergeable mergeStateStatus} }}` {
		t.Fatalf("query %s", q)
	}
	if q := withMerge(batchQuery("bic/aether", []int{7}, nil, false), []int{1}); !strings.Contains(q, "p7: pullRequest(number:7){") ||
		!strings.HasSuffix(q, "m1: pullRequest(number:1){number mergeable mergeStateStatus} }}") {
		t.Fatalf("one call for the re-read and the mergeability: %s", q)
	}
	if got := withRefs("{a0: x }", "bic/aether", []string{"main", "s/x"}); got !=
		`{a0: x bh: repository(owner:"bic", name:"aether"){r0: ref(qualifiedName:"refs/heads/main"){target{oid}} r1: ref(qualifiedName:"refs/heads/s/x"){target{oid}} } }` {
		t.Fatalf("withRefs %s", got)
	}
	if got := withRefs("{a0: x }", "bic/aether", nil); got != "{a0: x }" {
		t.Fatalf("no refs changed the query: %s", got)
	}
}

// A settled conflict is CONFLICTING or DIRTY (as states.py); UNKNOWN, BEHIND and BLOCKED never are.
func TestSettledConflict(t *testing.T) {
	for _, c := range []struct {
		mg, ms string
		want   bool
	}{{"CONFLICTING", "BLOCKED", true}, {"MERGEABLE", "DIRTY", true}, {"UNKNOWN", "UNKNOWN", false},
		{"MERGEABLE", "BEHIND", false}, {"MERGEABLE", "BLOCKED", false}} {
		if got := settledConflict(c.mg, c.ms); got != c.want {
			t.Errorf("%s/%s: %v", c.mg, c.ms, got)
		}
	}
}

// GHE quota (11-17): a pass that re-reads a PR and asks mergeability after a base move spends ONE call on both, not two.
func TestBaseWatchRidesTheReRead(t *testing.T) {
	w := newWorld(t)
	w.gh.items[201].base = "stack/x"
	w.gh.heads = map[string]string{"main": "a1aaaaaaaa", "stack/x": "b1bbbbbbbb"}
	w.pass(0)
	w.pass(2 * time.Minute) // bases first seen
	w.gh.heads["main"] = "a2aaaaaaaa"
	w.st.PRs[201].Settling = true // re-read this pass
	w.gh.light, w.gh.queries = nil, nil
	r := w.pass(2 * time.Minute)
	if w.gh.calls != 2 || len(w.gh.light) != 1 || !strings.Contains(w.gh.light[0], "m101:") || !strings.Contains(w.gh.light[0], "p201: pullRequest") {
		t.Fatalf("%d calls (want the search + one), light %q, log %v", w.gh.calls, w.gh.light, r.Log)
	}
}
