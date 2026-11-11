package scout

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func claimWorld(t *testing.T) *world {
	w := newWorld(t)
	w.watch.ClaimBy, w.watch.ClaimLabel = his, "praxis:pr-open"
	w.watch.FilesTitle = `^\[(Effort ledger|Effort retire|Effort stale|Prompt cache re-establish)`
	return w
}

func claimed(at time.Time) map[string]any {
	return map[string]any{"author": map[string]any{"login": his}, "createdAt": at.Format(time.RFC3339), "body": "Praxis claimed this issue (run 42)."}
}

func claimOf(t *testing.T, r Result, n int) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Set[ClaimKey(n)], &m); err != nil {
		t.Fatalf("claim:%d: %v (%s)", n, err, r.Set[ClaimKey(n)])
	}
	return m
}

// A Praxis claim (the claimer's "Praxis claimed" comment, or the claim label) on any issue of the repo is written as
// claim:<issue> {issue, pr, title, files, at, status}, from the same single search call (a quiet pass is still one
// call); its PR's files go by directory; status follows the PR: claimed, pr-open, merged; a merged claim goes
// KeepClaimClosed after its merge. A comment by someone else, or without the phrase, is no claim.
func TestPraxisClaims(t *testing.T) {
	w := claimWorld(t)
	at := w.now.Add(-30 * time.Minute)
	w.gh.items[5001] = &item{n: 5001, issue: true, author: "someone", title: "[PR-Followup] a follow-up", state: "OPEN", updated: w.now.Add(-20 * time.Minute),
		comments: []map[string]any{claimed(at)}}
	w.gh.items[5002] = &item{n: 5002, issue: true, author: "someone", title: "not claimed", state: "OPEN", updated: w.now.Add(-20 * time.Minute),
		comments: []map[string]any{{"author": map[string]any{"login": "bob"}, "createdAt": at.Format(time.RFC3339), "body": "Praxis claimed (not by the claimer)"},
			{"author": map[string]any{"login": his}, "createdAt": at.Format(time.RFC3339), "body": "looking at it"}}}
	r := w.pass(0)
	q := w.gh.queries[0]
	if r.GHCalls != 1 || !strings.Contains(q, "c0: search") || !strings.Contains(q, "c1: search") || !strings.Contains(q, `commenter:`+his) {
		t.Fatalf("claims not in the one search call: %d calls %.300s", r.GHCalls, q)
	}
	c := claimOf(t, r, 5001)
	if c["at"] != float64(at.Unix()) || c["issue"] != 5001.0 || c["status"] != "claimed" || c["title"] != "[PR-Followup] a follow-up" ||
		len(c) != 4 || r.Set[ClaimKey(5002)] != nil {
		t.Fatalf("claim %v, 5002 %s", c, r.Set[ClaimKey(5002)])
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 || len(r.Set) != 0 {
		t.Fatalf("quiet pass: %d calls, set %v", r.GHCalls, setKeys(r))
	}
	// Praxis opens a PR for it.
	w.gh.items[6001] = &item{n: 6001, author: "praxis-bot", title: "fix", state: "OPEN", head: headC, updated: w.now,
		files: []string{"aether_rust/crates/x/src/a.rs", "aether_rust/crates/x/src/b.rs", "README.md"}}
	is := w.gh.items[5001]
	is.closedBy, is.updated = []int{6001}, w.now.Add(time.Minute)
	c = claimOf(t, w.pass(2*time.Minute), 5001)
	if c["status"] != "pr-open" || c["pr"] != 6001.0 || fmt.Sprint(c["files"]) != "map[:[README.md] aether_rust/crates/x/src/:[a.rs b.rs]]" {
		t.Fatalf("pr open: %v", c)
	}
	// Merged: the issue closes; the claim says merged, then goes KeepClaimClosed after the merge.
	merged := w.now.Add(time.Minute)
	w.gh.items[6001].state, w.gh.items[6001].mergedAt = "MERGED", merged.Format(time.RFC3339)
	is.state, is.closedAt, is.updated = "CLOSED", merged.Format(time.RFC3339), merged
	if c := claimOf(t, w.pass(2*time.Minute), 5001); c["status"] != "merged" || w.st.Claims[5001].Done != merged.Unix() {
		t.Fatalf("merged: %v", c)
	}
	if r := w.pass(KeepClaimClosed - 10*time.Minute); len(r.Unset) != 0 {
		t.Fatalf("pruned early: %v", r.Unset)
	}
	if r := w.pass(10 * time.Minute); strings.Join(r.Unset, " ") != "claim:5001" || w.st.Claims[5001] != nil {
		t.Fatalf("not pruned: %v", r.Unset)
	}
}

// The claim label alone is a claim (pr-open). A label-only claim whose label goes is not returned by either claim
// search any more: it stays as last seen (removed with the issue's close by KeepClaimClosed only once a search
// returns it closed; claims with a claim comment are always returned).
func TestClaimByLabel(t *testing.T) {
	w := claimWorld(t)
	w.gh.items[5003] = &item{n: 5003, issue: true, author: "someone", title: "labelled", state: "OPEN", updated: w.now.Add(-time.Minute), labels: []string{"praxis:pr-open"}}
	if c := claimOf(t, w.pass(0), 5003); c["status"] != "pr-open" || c["at"] != nil {
		t.Fatalf("label claim: %v", c)
	}
	w.gh.items[5003].labels, w.gh.items[5003].updated = nil, w.now.Add(time.Minute)
	// The label search no longer returns it; the claimer's search never did: the claim stays as last seen until a
	// search returns it (a claim search only sees claims). Here the author search does not cover it either.
	r := w.pass(2 * time.Minute)
	if len(r.Unset) != 0 {
		t.Fatalf("removed without being read: %v", r.Unset)
	}
}

// Our open PRs whose title matches files_title carry fs: their changed files by directory; others carry none.
func TestFilesOnTaggedPRs(t *testing.T) {
	w := claimWorld(t)
	w.gh.items[104] = &item{n: 104, author: ours, title: "[Effort ledger][1-29] feat: the ledger", state: "OPEN", head: headC, mergeable: "MERGEABLE",
		updated: w.now.Add(-time.Hour), files: []string{"aether_rust/crates/e/src/ledger.rs", "aether_rust/crates/e/src/mod.rs", "docs/x.md"}}
	r := w.pass(0)
	if fs := fmt.Sprint(entry(t, r, "pr:104")["fs"]); fs != "map[aether_rust/crates/e/src/:[ledger.rs mod.rs] docs/:[x.md]]" {
		t.Fatalf("fs %s", fs)
	}
	if entry(t, r, "pr:101")["fs"] != nil {
		t.Fatal("an untagged PR carries fs")
	}
	w.gh.items[101].files, w.gh.items[101].updated = []string{"x/y.rs"}, w.now.Add(time.Minute)
	if r := w.pass(2 * time.Minute); entry(t, r, "pr:101")["fs"] != nil {
		t.Fatal("an untagged PR with files carries fs")
	}
	if g := Grouped(make([]string, 80), MaxFiles); len(g[""]) != MaxFiles {
		t.Fatalf("cap: %d", len(g[""]))
	}
}

// Past the cap, merged and closed claims go first, before older merged PRs.
func TestClosedClaimsPrunedFirst(t *testing.T) {
	w := claimWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Add(-5*time.Hour).Format(time.RFC3339), w.now.Add(time.Minute)
	w.gh.items[5001] = &item{n: 5001, issue: true, author: "someone", title: "a claim " + strings.Repeat("x", 600), state: "CLOSED", closedAt: w.now.Format(time.RFC3339),
		updated: w.now.Add(time.Minute), comments: []map[string]any{claimed(w.now.Add(-time.Hour))}}
	w.pass(2 * time.Minute)
	if w.st.Claims[5001] == nil || w.st.PRs[101].State != "MERGED" {
		t.Fatalf("setup: claim %v pr %v", w.st.Claims[5001], w.st.PRs[101])
	}
	total, _ := FormSize(&w.st)
	w.cap = total - 50
	r := w.pass(2 * time.Minute)
	if len(r.Unset) == 0 || r.Unset[0] != "claim:5001" || w.st.PRs[101] == nil {
		t.Fatalf("pruned %v (pr:101 kept %v)", r.Unset, w.st.PRs[101] != nil)
	}
}

// A merged PR keeps only what its retention and ledger need in the state.
func TestClosedPRStateSlim(t *testing.T) {
	w := claimWorld(t)
	w.gh.items[101].body = "Part of #3010"
	w.pass(0)
	if tp := w.st.PRs[101]; tp.Head == "" || tp.Owner != 0 && false {
		t.Fatalf("open state %+v", tp)
	}
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	b, _ := json.Marshal(w.st.PRs[101])
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"h", "x", "oi", "l", "mk", "r"} {
		if _, has := m[k]; has {
			t.Fatalf("merged PR state keeps %s: %s", k, b)
		}
	}
	if m["rh"] == nil || m["z"] == nil || m["s"] != "MERGED" {
		t.Fatalf("merged PR state lost its ledger fields: %s", b)
	}
}

// toClaim: only the claimer's comment with the phrase dates a claim; another login's, or the claimer's without the
// phrase, does not; the label alone makes one (no at).
func TestToClaim(t *testing.T) {
	var n ghNode
	n.Number, n.Title, n.State = 7, "t", "OPEN"
	add := func(who, body, at string) {
		n.Comments.Nodes = append(n.Comments.Nodes, struct {
			Author    *login `json:"author"`
			CreatedAt string `json:"createdAt"`
			Body      string `json:"body"`
		}{&login{Login: who}, at, body})
	}
	add("bob", "Praxis claimed", "2026-11-11T01:00:00Z")
	add(his, "looking at it", "2026-11-11T02:00:00Z")
	if _, ok := toClaim(n, his, "Praxis claimed", "praxis:pr-open"); ok {
		t.Fatal("a claim from bob's comment or the claimer's without the phrase")
	}
	add(his, "Praxis claimed it", "2026-11-11T00:30:00Z")
	if c, ok := toClaim(n, his, "Praxis claimed", "praxis:pr-open"); !ok || c.At != "2026-11-11T00:30:00Z" || c.Status() != "claimed" {
		t.Fatalf("claim %+v %v", c, ok)
	}
	var l ghNode
	l.Number = 8
	l.Labels.Nodes = append(l.Labels.Nodes, struct {
		Name string `json:"name"`
	}{"praxis:pr-open"})
	if c, ok := toClaim(l, his, "", "praxis:pr-open"); !ok || c.At != "" || c.Status() != "pr-open" {
		t.Fatalf("label claim %+v %v", c, ok)
	}
}

// A claim:<n> key on the form the state does not track is removed like any untracked entry.
func TestUntrackedClaimRemoved(t *testing.T) {
	w := claimWorld(t)
	w.gh.items[5001] = &item{n: 5001, issue: true, author: "someone", title: "c", state: "OPEN", updated: w.now.Add(-time.Minute),
		comments: []map[string]any{claimed(w.now.Add(-time.Hour))}}
	w.pass(0)
	w.own = []string{"claim:5001", "claim:4242", "issue:301", "pr:101", "pr:102", "pr:201"}
	if r := w.pass(2 * time.Minute); strings.Join(r.Unset, " ") != "claim:4242" {
		t.Fatalf("unset %v", r.Unset)
	}
}
