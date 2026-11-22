package ghcache

import (
	"encoding/json"

	"fmt"
	"github.com/gluck/ebac/sdk/forge"
	"strings"
	"testing"
	"time"
)

// Review r1 blocking 1: a number that does not resolve (deleted, or an issue) is a NOT_FOUND on its alias, never a
// failed round: the others are read and written, the missing one is dropped (and its form retired once it had one).
// RED if aliasErrs stops treating a [rN, pN] NOT_FOUND as missing (every round fails "marks read").
func TestANotFoundPRDoesNotWedgeTheRound(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"repo":"bic/aether","prs":[131611,99999999]}]}`)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 2 || keys(r) != "aether-pr-131611" || !strings.Contains(strings.Join(r.Log, "\n"), "aether-pr-99999999: not found (deleted, transferred or not a PR): dropped") {
		t.Fatalf("missing explicit PR: %d calls, writes %q, log %q", calls, keys(r), r.Log)
	}
	// A tracked PR with a form that stops resolving: dropped and its form retired.
	delete(gh.prs["bic/aether"], 131611)
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if keys(r) != "retire:aether-pr-131611" || len(r.Track) != 0 {
		t.Fatalf("vanished PR: writes %q, track %v", keys(r), r.Track)
	}
	// Any other error still fails the round, by its message.
	if _, _, other := aliasErrs([]forgeErr{{Type: "FORBIDDEN", Message: "no", Path: []string{"s0"}}}, nil); other == nil ||
		other.Error() != "graphql: FORBIDDEN no at s0" {
		t.Fatalf("other error: %v", other)
	}
	// An error under a PR alias is that PR's, for its sync.errors.
	_, per, other := aliasErrs([]forgeErr{{Type: "FORBIDDEN", Message: "x", Path: []string{"r0", "p7", "closingIssuesReferences"}}}, map[string]string{"r0": "bic/aether"})
	if other != nil || strings.Join(per["aether-pr-7"], "|") != "closingIssuesReferences: FORBIDDEN x" {
		t.Fatalf("per-PR error: %v %v", per, other)
	}
}

// Explicit numbers belong to their rule's repo only: with two repos, rule {repo: bic/aether, prs: [131611]} never asks
// the other repo for 131611. RED if prs fan out to every repo again.
func TestExplicitPRsAreScopedToTheirRepo(t *testing.T) {
	gh := world(t)
	gh.prs["bic/cowork"] = map[int]*PR{}
	c, ttl := cfg(t, `{"repos":["bic/aether","bic/cowork"],"rules":[{"repo":"bic/aether","prs":[131611]}]}`)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if keys(r) != "aether-pr-131611" || strings.Contains(gh.calls[0], `name:"cowork"`) {
		t.Fatalf("writes %q, marks query %s", keys(r), gh.calls[0])
	}
}

// Review r1 blocking 2: at most config.max forms. Five matching PRs with max 3: three admitted (the newest numbers
// of the first rule), the rest logged; an ended PR's form is retired before a new one is refused. RED if the cap goes.
func TestTheFormCap(t *testing.T) {
	gh := world(t)
	base := gh.prs["bic/aether"][127740]
	var ns []int
	for i := 1; i <= 5; i++ {
		pr := *base
		pr.Number = 300000 + i
		gh.prs["bic/aether"][pr.Number] = &pr
		ns = append(ns, pr.Number)
	}
	q := "repo:bic/aether is:pr is:open author:John-Kelliher"
	gh.search[q] = ns
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"author":["John-Kelliher"]}],"max":3,"ttl":{"closed":"1h"}}`)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if keys(r) != "aether-pr-300003,aether-pr-300004,aether-pr-300005" ||
		!strings.Contains(strings.Join(r.Log, "\n"), "cap 3 forms: 2 PR(s) not tracked: aether-pr-300002,aether-pr-300001") {
		t.Fatalf("cap: writes %q log %q", keys(r), r.Log)
	}
	// 300005 merges; a new 300006 arrives: the merged form retires early (inside its ttl) to admit it.
	gh.prs["bic/aether"][300005].State = "MERGED"
	pr := *base
	pr.Number = 300006
	gh.prs["bic/aether"][300006] = &pr
	gh.search[q] = []int{300003, 300004, 300006}
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if !strings.Contains(keys(r), "aether-pr-300006") || !strings.Contains(keys(r), "retire:aether-pr-300005") || len(gh.children) != 3 {
		t.Fatalf("evict ended: writes %q, children %v", keys(r), gh.children)
	}
	if DefaultMax != 80 || MaxMax != 90 {
		t.Fatalf("DefaultMax %d MaxMax %d", DefaultMax, MaxMax)
	}
}

// Review r1 blocking 3: writes past runtime.forms_landed did not land, so the next round reads and writes them again
// though no mark moved; a retire that did not land is sent again. RED if Redo goes (the PR's form stays stale).
func TestUnlandedWritesAreRedone(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	gh.failLand = 1 // the second write fails
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if gh.landed != 1 || len(gh.children) != 1 {
		t.Fatalf("landed %d children %v", gh.landed, gh.children)
	}
	gh.failLand = -1
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(time.Minute))
	if calls != 1 || keys(r) != "aether-pr-127740" || !strings.Contains(strings.Join(r.Log, "\n"), "1 of 2 form write(s) of the last round did not land: redone") {
		t.Fatalf("redo: %d calls, writes %q, log %q", calls, keys(r), r.Log)
	}
	// A retire that does not land: the child is still held, so the next round retires it again.
	gh.children["aether-pr-1"] = "@orphan"
	gh.failLand = 0
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(2*time.Minute))
	if keys(r) != "retire:aether-pr-1" {
		t.Fatalf("orphan retire: %q", keys(r))
	}
	gh.failLand = -1
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(3*time.Minute))
	if keys(r) != "retire:aether-pr-1" || gh.children["aether-pr-1"] != "" {
		t.Fatalf("orphan retire again: %q, children %v", keys(r), gh.children)
	}
}

// Review r1 blocking 4: reviews and comments are read newest 30; more on GitHub is named in sync.errors, as threads
// are. RED if the totalCount check goes.
func TestTruncatedReadsAreNamed(t *testing.T) {
	pr := fixturePRs(t)[116018]
	// The capture predates totalCount in the query: the counts GitHub would give a PR this busy.
	pr.Reviews.TotalCount, pr.Comments.TotalCount = 45, 40
	if len(pr.Reviews.Nodes) != 30 || len(pr.Comments.Nodes) != 30 || len(pr.Threads.Nodes) != 12 {
		t.Fatalf("fixture: reviews %d, comments %d, threads %d", len(pr.Reviews.Nodes), len(pr.Comments.Nodes), len(pr.Threads.Nodes))
	}
	fw, _, _ := entityWrite("k", pr, GHOf(pr), nil, &Entity{}, t0)
	var sy SyncKey
	if err := json.Unmarshal(fw.Set["sync"], &sy); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("truncated: threads: 12 of %d read (the newest)", pr.Threads.TotalCount),
		"truncated: reviews: 30 of 45 read (the newest)",
		"truncated: comments: 30 of 40 read (the newest)",
	}
	got := strings.Join(sy.Errors, "|")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("sync.errors %q lacks %q", got, w)
		}
	}
}

// A search past SearchFirst answers only its first 100: nothing is untracked that round. RED if the PRs past 100 are
// marked UNTRACKED (and retired a ttl later) though they still match.
func TestAnIncompleteSearchUntracksNothing(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	q := "repo:bic/aether is:pr is:open author:John-Kelliher"
	gh.search[q] = []int{116018}
	gh.searchTotal = 101
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if got := r.Track["bic/aether#127740"].State; got != "OPEN" {
		t.Fatalf("127740 after an incomplete search: %q", got)
	}
}

type forgeErr = forge.GQLError

// Review r2: a failed UPDATE of a child the core already holds is rewritten next round, though gh+parsed equal what
// was sent. RED if the unlanded write's digest is kept (mutant N8): the redo read finds "unchanged" and writes nothing.
func TestAnUnlandedUpdateOfAHeldChildIsRewritten(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if len(gh.children) != 2 {
		t.Fatalf("children %v", gh.children)
	}
	gh.prs["bic/aether"][116018].UpdatedAt = "2026-11-20T04:04:00Z"
	gh.prs["bic/aether"][127740].UpdatedAt = "2026-11-20T04:04:00Z"
	gh.failLand = 0 // neither update lands
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if keys(r) != "aether-pr-116018,aether-pr-127740" || gh.landed != 0 {
		t.Fatalf("updates: writes %q, landed %d", keys(r), gh.landed)
	}
	gh.failLand = -1
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(HotEvery+time.Minute))
	if calls != 1 || keys(r) != "aether-pr-116018,aether-pr-127740" {
		t.Fatalf("rewrite: %d calls, writes %q", calls, keys(r))
	}
	var g GH
	if err := json.Unmarshal(r.Forms[0].Set["gh"], &g); err != nil || g.UpdatedAt != "2026-11-20T04:04:00Z" {
		t.Fatalf("rewritten gh: %+v %v", g.UpdatedAt, err)
	}
}
