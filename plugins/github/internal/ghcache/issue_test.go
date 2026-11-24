package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// #45: issue entity forms. One form per tracked issue (<repo>-issue-<n>), gh.kind issue, the conversation
// comments by node id, no PR-only key; a PR form carries gh.kind pr, its comment bodies and each thread's newest
// comment bodies. Fixtures are synthetic (example/repo, authors someone / ours, ids 0000000N).

func synthIssue(n int, author, body string, comments ...Comment) *Issue {
	is := &Issue{ID: fmt.Sprintf("I_%08d", n), Number: n, URL: fmt.Sprintf("https://example.invalid/example/repo/issues/%d", n),
		Title: fmt.Sprintf("[topic][1-2] issue %d", n), Body: body, State: "OPEN", UpdatedAt: "2026-11-20T04:00:00Z",
		CreatedAt: "2026-11-19T04:00:00Z", Author: &Login{Login: author}}
	is.Comments.Nodes = comments
	is.Comments.TotalCount = len(comments)
	return is
}

func cmt(id, author, body string) Comment {
	return Comment{ID: id, Author: &Login{Login: author}, Body: body, CreatedAt: "2026-11-19T05:00:00Z", UpdatedAt: "2026-11-19T05:00:00Z",
		URL: "https://example.invalid/example/repo/issues/5#" + id}
}

func issueWorld(t *testing.T, issues ...*Issue) *fakeGH {
	m := map[int]*Issue{}
	for _, is := range issues {
		m[is.Number] = is
	}
	return &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{},
		prs: map[string]map[int]*PR{"example/repo": {}}, issues: map[string]map[int]*Issue{"example/repo": m}}
}

const issueRule = `{"repos":["example/repo"],"rules":[{"repo":"example/repo","issues":[5]}]}`

func ghKeys(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func formOf(t *testing.T, r Result, key string) map[string]json.RawMessage {
	t.Helper()
	for _, f := range r.Forms {
		if f.Key == key && !f.Retire {
			return f.Set
		}
	}
	t.Fatalf("no write of %s: %s", key, keys(r))
	return nil
}

func TestAnIssueEntityHasCommentsAndNoPROnlyKeys(t *testing.T) {
	gh := issueWorld(t, synthIssue(5, "Someone", "## Part of\n#4\n", cmt("00000001", "Someone", "first"), cmt("00000002", "ours", "second")))
	c, ttl := cfg(t, issueRule)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 2 || keys(r) != "repo-issue-5" || gh.lsrefs != 0 {
		t.Fatalf("calls %d, writes %q, ls-refs %d (want 2, repo-issue-5, 0: ls-refs knows pull heads only)", calls, keys(r), gh.lsrefs)
	}
	if !strings.Contains(gh.calls[0], "i5: issue(number:5){number updatedAt state comments{totalCount}}") {
		t.Errorf("marks: %s", gh.calls[0])
	}
	if !strings.Contains(gh.calls[1], "i5: issue(number:5){...I}") || !strings.Contains(gh.calls[1], "fragment I on Issue {") ||
		strings.Contains(gh.calls[1], "fragment D on PullRequest") {
		t.Errorf("deep: %s", gh.calls[1])
	}
	set := formOf(t, r, "repo-issue-5")
	var got []string
	for k := range ghKeys(t, set["gh"]) {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"author", "body", "comments", "comments_total", "created_at", "kind", "number", "state", "title", "updated_at", "url"}
	if !slices.Equal(got, want) {
		t.Errorf("issue gh keys %v, want %v (no PR-only key)", got, want)
	}
	var g GH
	_ = json.Unmarshal(set["gh"], &g)
	wantC := map[string]CachedComment{
		"00000001": {Author: "someone", Body: "first", CreatedAt: "2026-11-19T05:00:00Z", UpdatedAt: "2026-11-19T05:00:00Z", URL: "https://example.invalid/example/repo/issues/5#00000001"},
		"00000002": {Author: "ours", Body: "second", CreatedAt: "2026-11-19T05:00:00Z", UpdatedAt: "2026-11-19T05:00:00Z", URL: "https://example.invalid/example/repo/issues/5#00000002"},
	}
	if g.Kind != "issue" || g.Number != 5 || g.Author != "someone" || g.CommentsTotal != 2 || g.Body.Text != "## Part of\n#4\n" || !mapsEqual(g.Comments, wantC) {
		t.Errorf("issue gh %+v", g)
	}
	var ps Parsed
	_ = json.Unmarshal(set["parsed"], &ps)
	if !slices.Equal(ps.Links.PartOf, []int{4}) || ps.Stack == nil || ps.Stack.K != 1 || ps.Stack.N != 2 {
		t.Errorf("issue parsed %s", set["parsed"])
	}
	if tr := r.Track["example/repo#5"]; tr.Form != "repo-issue-5" || tr.State != "OPEN" {
		t.Errorf("track %+v", r.Track)
	}
}

func mapsEqual(a, b map[string]CachedComment) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestAnUnchangedIssueCostsNoDeepReadAndAnUpstreamCommentEditIsPulled(t *testing.T) {
	is := synthIssue(5, "someone", "body", cmt("00000001", "someone", "before"))
	gh := issueWorld(t, is)
	c, ttl := cfg(t, issueRule)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if calls != 1 || len(r.Forms) != 0 {
		t.Fatalf("unchanged issue at its due round: %d calls, writes %q (want 1 marks call, none)", calls, keys(r))
	}
	is.Comments.Nodes[0].Body, is.Comments.Nodes[0].UpdatedAt, is.UpdatedAt = "after", "2026-11-20T05:00:00Z", "2026-11-20T05:00:00Z"
	r, calls = round(t, gh, r.State, c, ttl, t0.Add(2*HotEvery))
	var g GH
	_ = json.Unmarshal(formOf(t, r, "repo-issue-5")["gh"], &g)
	if calls != 2 || g.Comments["00000001"].Body != "after" || g.Comments["00000001"].UpdatedAt != "2026-11-20T05:00:00Z" {
		t.Fatalf("an upstream comment edit: %d calls, comment %+v", calls, g.Comments["00000001"])
	}
}

func TestANewIssueCommentAloneMovesTheMark(t *testing.T) {
	is := synthIssue(5, "someone", "body")
	gh := issueWorld(t, is)
	c, ttl := cfg(t, issueRule)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	is.Comments.Nodes = append(is.Comments.Nodes, cmt("00000003", "someone", "new"))
	is.Comments.TotalCount = 1 // updatedAt unchanged: the count alone moves the mark
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	var g GH
	_ = json.Unmarshal(formOf(t, r, "repo-issue-5")["gh"], &g)
	if calls != 2 || g.Comments["00000003"].Body != "new" {
		t.Fatalf("a new comment: %d calls, comments %+v", calls, g.Comments)
	}
}

func TestTruncatedIssueCommentsAreFlagged(t *testing.T) {
	is := synthIssue(5, "someone", "body", cmt("00000001", "someone", "a"), cmt("00000002", "someone", "b"))
	is.Comments.TotalCount = 40
	gh := issueWorld(t, is)
	c, ttl := cfg(t, issueRule)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	var sy SyncKey
	_ = json.Unmarshal(formOf(t, r, "repo-issue-5")["sync"], &sy)
	if !slices.Contains(sy.Errors, "truncated: comments: 2 of 40 read (the newest)") {
		t.Fatalf("sync.errors %q", sy.Errors)
	}
}

func TestAMissingIssueIsDroppedNotARoundFailure(t *testing.T) {
	gh := issueWorld(t)
	c, ttl := cfg(t, issueRule)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 1 || len(r.Forms) != 0 || !strings.Contains(strings.Join(r.Log, "\n"), "repo-issue-5: not found (deleted, transferred or not an issue): dropped") {
		t.Fatalf("missing issue: %d calls, writes %q, log %q", calls, keys(r), r.Log)
	}
}

// An issue form's edit is not written: issue writes come with the issue write path (#71 A2, #64). Until then the
// sync names the refusal and keeps GitHub's value.
func TestAnIssueFormEditIsRefusedAndNothingWritten(t *testing.T) {
	gh := issueWorld(t, synthIssue(5, "someone", "body"))
	gh.wr = &fakeWrites{repoLabels: map[string]bool{"x": true}}
	c, ttl := cfg(t, issueRule)
	w := &writeWorld{t: t, gh: gh, c: c, ttl: ttl, forms: map[string]map[string]json.RawMessage{}, now: t0,
		critic: &model.Critic{Name: "ghc", Source: model.SourceGHCache, Write: true, GHWrite: &model.GHWrite{
			Scope:  model.GHScope{Repos: []string{"example/repo"}, Authors: []string{"someone"}, IssuesFixedByScope: true},
			Fields: model.GHWriteFields}, Maintainer: trustedMaint}}
	w.round()
	w.edit("repo-issue-5", func(g map[string]any) { g["labels"] = []string{"x"} })
	w.round()
	sy := w.formSync("repo-issue-5")
	if a := sy.Alarm[AlarmWriteRefused]["labels"]; !strings.Contains(a.Detail, "issue writes are not built yet") {
		t.Fatalf("alarm %+v", sy.Alarm)
	}
	if m := w.mutations(); len(m) != 0 || len(w.formGH("repo-issue-5").Labels) != 0 {
		t.Fatalf("mutations %q, labels %v", m, w.formGH("repo-issue-5").Labels)
	}
}

func TestAPRFormCarriesKindCommentBodiesAndThreadComments(t *testing.T) {
	pr := synthPR(t, 7, "someone", nil, false, 1)
	pr.Comments.Nodes = []Comment{cmt("00000011", "someone", "a conversation comment")}
	pr.Comments.TotalCount = 1
	th := &pr.Threads.Nodes[0]
	th.Comments.TotalCount = 5
	th.Comments.Nodes = []Comment{cmt("00000021", "someone", "third"), cmt("00000022", "ours", "fourth"), cmt("00000023", "someone", "fifth")}
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{}, prs: map[string]map[int]*PR{"example/repo": {7: pr}}}
	c, ttl := cfg(t, `{"repos":["example/repo"],"rules":[{"repo":"example/repo","prs":[7]}]}`)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	var g GH
	_ = json.Unmarshal(formOf(t, r, "repo-pr-7")["gh"], &g)
	tc := g.Threads["PRRT_7_1"].Comments
	if g.Kind != "pr" || g.Comments["00000011"].Body != "a conversation comment" || g.CommentsTotal != 1 ||
		len(tc) != 3 || tc["00000022"].Body != "fourth" || tc["00000022"].Author != "ours" || g.Threads["PRRT_7_1"].N != 5 || g.Body.Text != pr.Body {
		t.Fatalf("pr gh: kind %q comments %+v threads %+v body %q", g.Kind, g.Comments, g.Threads, g.Body.Text)
	}
	if !strings.Contains(gh.calls[1], "comments(last:3){totalCount nodes{id author{login} body createdAt updatedAt url}}") {
		t.Errorf("deep thread selection: %s", gh.calls[1])
	}
}

// Over the 16 KB cap the texts go first and ours last (#45): others' comment bodies are cut to SectionHead and
// marked cut; ours stays whole; the threads and sections stay.
func TestOthersTextsAreCutBeforeOursAndBeforeThreads(t *testing.T) {
	long := strings.Repeat("x", 600)
	var cs []Comment
	for i := 1; i <= 30; i++ {
		cs = append(cs, cmt(fmt.Sprintf("%08d", i), "someone", long))
	}
	cs = append(cs, cmt("00000099", "Ours", long))
	gh := issueWorld(t, synthIssue(5, "someone", "body", cs...))
	c, ttl := cfg(t, issueRule)
	r, err := Run(context.Background(), gh, State{}, Pass{Config: c, ClosedTTL: ttl, Now: t0, Children: map[string]string{}, Ours: "ours"})
	if err != nil {
		t.Fatal(err)
	}
	set := formOf(t, r, "repo-issue-5")
	if n := len(set["gh"]) + len(set["parsed"]) + len(set["sync"]); n > MaxForm {
		t.Fatalf("%d bytes", n)
	}
	var g GH
	var sy SyncKey
	_ = json.Unmarshal(set["gh"], &g)
	_ = json.Unmarshal(set["sync"], &sy)
	o, s1 := g.Comments["00000099"], g.Comments["00000001"]
	if o.Body != long || o.Cut || len(s1.Body) != SectionHead || !s1.Cut || !slices.Equal(sy.Trimmed, []string{"comments"}) {
		t.Fatalf("ours %d bytes cut %v, someone %d bytes cut %v, trimmed %v", len(o.Body), o.Cut, len(s1.Body), s1.Cut, sy.Trimmed)
	}
}

func TestParseConfigIssues(t *testing.T) {
	for _, bad := range []string{
		`{"repos":["example/repo"],"rules":[{"issues":[5]}]}`,
		`{"repos":["example/repo"],"rules":[{"repo":"example/repo","issues":[0]}]}`,
		`{"repos":["example/repo"],"rules":[{"repo":"example/repo","prs":[5],"issues":[5]}]}`,
	} {
		if _, _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, _, err := ParseConfig(json.RawMessage(issueRule))
	if err != nil || !slices.Equal(c.Rules[0].Issues, []int{5}) {
		t.Fatalf("issues-only rule: %v %v", c, err)
	}
	if IssueKey("example/repo", 5) != "repo-issue-5" || ChildKey("example/repo", 5) != "repo-pr-5" {
		t.Fatal("keys")
	}
}

// Ours deep in a long thread (#45, the knight's L1 change (a)): a thread longer than ThreadLast is read whole once
// per comment count and our comments in it join the form's thread comments; the next round at the same count reads
// nothing more; a new comment (count moves) reads it whole again. Others' older comments stay off the form.
func TestOursDeepInALongThreadIsReadWholeOncePerCount(t *testing.T) {
	pr := synthPR(t, 7, "someone", nil, false, 1)
	th := &pr.Threads.Nodes[0]
	all := []Comment{cmt("00000031", "someone", "one"), cmt("00000032", "ours", "two, ours"), cmt("00000033", "someone", "three"),
		cmt("00000034", "someone", "four"), cmt("00000035", "someone", "five")}
	th.Comments.TotalCount, th.Comments.Nodes = len(all), all[2:]
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{}, prs: map[string]map[int]*PR{"example/repo": {7: pr}},
		whole: map[string][]Comment{"PRRT_7_1": all}}
	c, ttl := cfg(t, `{"repos":["example/repo"],"rules":[{"repo":"example/repo","prs":[7]}]}`)
	run := func(st State, at time.Time) Result {
		t.Helper()
		r, err := Run(context.Background(), gh, st, Pass{Config: c, ClosedTTL: ttl, Now: at, Children: map[string]string{}, Ours: "ours"})
		if err != nil {
			t.Fatal(err)
		}
		var back State
		_ = json.Unmarshal(mustJSON(r.State), &back)
		r.State = back
		return r
	}
	wholeReads := func() int {
		n := 0
		for _, q := range gh.calls {
			if strings.Contains(q, `t0: node(id:"PRRT_7_1"){... on PullRequestReviewThread{comments(first:100)`) {
				n++
			}
		}
		return n
	}
	r := run(State{}, t0)
	var g GH
	_ = json.Unmarshal(formOf(t, r, "repo-pr-7")["gh"], &g)
	tc := g.Threads["PRRT_7_1"].Comments
	if wholeReads() != 1 || len(tc) != 4 || tc["00000032"].Body != "two, ours" || tc["00000031"].Body != "" {
		t.Fatalf("whole reads %d, thread comments %+v", wholeReads(), tc)
	}
	pr.UpdatedAt = "2026-11-20T05:00:00Z" // the mark moves, the thread does not
	r = run(r.State, t0.Add(HotEvery))
	_ = json.Unmarshal(formOf(t, r, "repo-pr-7")["gh"], &g)
	if wholeReads() != 1 || g.Threads["PRRT_7_1"].Comments["00000032"].Body != "two, ours" {
		t.Fatalf("same count: whole reads %d, thread comments %+v", wholeReads(), g.Threads["PRRT_7_1"].Comments)
	}
	all = append(all, cmt("00000036", "someone", "six"))
	gh.whole["PRRT_7_1"] = all
	th.Comments.TotalCount, th.Comments.Nodes = len(all), all[3:]
	pr.UpdatedAt = "2026-11-20T06:00:00Z"
	r = run(r.State, t0.Add(2*HotEvery))
	if wholeReads() != 2 {
		t.Fatalf("a new comment: whole reads %d, want 2", wholeReads())
	}
}

// Without an ours login nothing is read whole (a critic with no trusted ours_login pays nothing).
func TestNoOursLoginReadsNoThreadWhole(t *testing.T) {
	pr := synthPR(t, 7, "someone", nil, false, 1)
	pr.Threads.Nodes[0].Comments.TotalCount = 9
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{}, prs: map[string]map[int]*PR{"example/repo": {7: pr}}}
	c, ttl := cfg(t, `{"repos":["example/repo"],"rules":[{"repo":"example/repo","prs":[7]}]}`)
	_, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 2 {
		t.Fatalf("calls %d (want marks + deep)", calls)
	}
}
