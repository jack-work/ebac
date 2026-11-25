package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// fakeCommentGH holds comments by node id as GitHub would answer the
// comment read, and applies the edit mutation to them.
type fakeCommentGH struct {
	comments  map[string]*fakeComment
	mutations []string
	failEdit  error
	dropEdit  bool // the mutation "succeeds" but GitHub keeps the old body
	reads     int
}

type fakeComment struct {
	review           bool
	body, author     string
	mine             bool
	repo             string
	onPR             bool
	n                int
	onAuthor, onRepo string
}

var nodeArg = regexp.MustCompile(`node\(id:"([^"]+)"\)`)

func (f *fakeCommentGH) GraphQL(_ context.Context, q string) (json.RawMessage, error) {
	f.reads++
	m := nodeArg.FindStringSubmatch(q)
	if m == nil {
		return nil, fmt.Errorf("unexpected query %q", q)
	}
	c := f.comments[m[1]]
	if c == nil {
		return json.RawMessage(`{"node":null}`), nil
	}
	parent := map[string]any{"number": c.n, "author": map[string]string{"login": c.onAuthor}, "repository": map[string]string{"nameWithOwner": c.onRepo}}
	node := map[string]any{"body": c.body, "viewerDidAuthor": c.mine, "author": map[string]string{"login": c.author},
		"repository": map[string]string{"nameWithOwner": c.repo}}
	if c.review {
		node["__typename"] = "PullRequestReviewComment"
		node["pullRequest"] = parent
	} else {
		node["__typename"] = "IssueComment"
		node["issue"] = parent // GitHub answers issue{} for a PR conversation comment too
		if c.onPR {
			node["pullRequest"] = parent
		} else {
			node["pullRequest"] = nil
		}
	}
	return json.Marshal(map[string]any{"node": node})
}

func (f *fakeCommentGH) Mutate(_ context.Context, m string, vars map[string]string) (json.RawMessage, error) {
	name := "updateIssueComment"
	if strings.Contains(m, "updatePullRequestReviewComment") {
		name = "updatePullRequestReviewComment"
	}
	f.mutations = append(f.mutations, name+" "+vars["c"]+" b="+vars["b"])
	if f.failEdit != nil {
		return nil, f.failEdit
	}
	if c := f.comments[vars["c"]]; c != nil && !f.dropEdit {
		c.body = vars["b"]
	}
	return json.RawMessage(`{}`), nil
}

// Synthesized fixtures only (a public repo): repo example/repo, logins "ours" / "someone". The trusted authority is
// the live github-cache critic's shape with comment_edit granted.
func ghCacheCritic() *model.Critic {
	return &model.Critic{Name: "gh-cache", Write: true, GHWrite: &model.GHWrite{
		Ours:   "ours",
		Fields: []string{"thread_reply", "thread_resolve", "labels", "draft", "issue_create", "comment_create", "comment_edit"},
		Scope:  model.GHScope{Repos: []string{"example/repo"}, Authors: []string{"ours"}, ReadOnlyAuthors: []string{"someone"}, IssuesFixedByScope: true},
	}}
}

// The incident's shape: a review comment on a merged PR naming an internal plan (a disclosure audit finding).
const leakBody = "No production caller until the wiring PR lands; tracked with the batch (an internal plan name)."
const scrubBody = "No production caller until the wiring PR lands; tracked in #1000."

func ourReviewComment() *fakeComment {
	return &fakeComment{review: true, body: leakBody, author: "ours", mine: true, repo: "example/repo", n: 1001, onAuthor: "ours", onRepo: "example/repo"}
}

const ourID = "PRRC_00000003"

// sha256(leakBody)[:6] in hex, written down (computed outside the code under test).
const leakHash = "cdcc073f4531"

func TestTheLeakHashIsTheCacheHash12(t *testing.T) {
	if got := bodyHash(leakBody); got != leakHash {
		t.Fatalf("bodyHash = %s, want %s", got, leakHash)
	}
}

// An edit of our review comment on a merged PR lands with the review mutation and the read-back holds it.
func TestAnEditOfOurCommentOnAMergedPRLandsAndReadsBack(t *testing.T) {
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: ourReviewComment()}}
	out, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, Body: scrubBody, Expect: leakHash})
	if err != nil {
		t.Fatal(err)
	}
	want := "PRRC_00000003 on example/repo#1001: edited cdcc073f4531 -> " + bodyHash(scrubBody) + ", read back"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if len(gh.mutations) != 1 || gh.mutations[0] != "updatePullRequestReviewComment "+ourID+" b="+scrubBody {
		t.Fatalf("mutations = %q", gh.mutations)
	}
	if gh.reads != 2 {
		t.Fatalf("reads = %d, want 2 (before the write, then the read-back)", gh.reads)
	}
}

// A conversation comment on a PR is edited with the issue-comment mutation, authorized as the PR.
func TestAConversationCommentOnAPRUsesTheIssueCommentMutation(t *testing.T) {
	c := ourReviewComment()
	c.review, c.onPR = false, true
	gh := &fakeCommentGH{comments: map[string]*fakeComment{"IC_00000001": c}}
	if _, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: "IC_00000001", Body: scrubBody, Expect: leakHash}); err != nil {
		t.Fatal(err)
	}
	if len(gh.mutations) != 1 || gh.mutations[0] != "updateIssueComment IC_00000001 b="+scrubBody {
		t.Fatalf("mutations = %q", gh.mutations)
	}
}

// Every refusal writes nothing and names its reason exactly.
func TestRefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(c *fakeComment, w *model.Critic, r *commentEditReq)
		reason string
	}{
		{"a foreign author", func(c *fakeComment, _ *model.Critic, _ *commentEditReq) { c.author = "someone" },
			"refused: PRRC_00000003 on example/repo#1001 is someone's, not ours (ours): another author's text is never edited"},
		{"no author (a deleted account)", func(c *fakeComment, _ *model.Critic, _ *commentEditReq) { c.author = "" },
			"refused: PRRC_00000003 on example/repo#1001 is an unknown author's, not ours (ours): another author's text is never edited"},
		{"ours by login but not this account", func(c *fakeComment, _ *model.Critic, _ *commentEditReq) { c.mine = false },
			"refused: GitHub says this account did not write PRRC_00000003 (viewerDidAuthor false)"},
		{"a body naming an internal term", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) {
			r.Body = "Passes; 0a1b2c3d is the seat that ran it."
		}, `refused: the new body names an internal term "0a1b2c3d is the seat" (rule \b[0-9a-f]{8}(?:[0-9a-f]{24})?\b(?=.{0,40}\b(aria|seat|figaro)\b)): GHE text may name figaro and nothing else internal; rephrase in broadly known terms`},
		{"a stale expect", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) { r.Expect = "000000000000" },
			`refused: PRRC_00000003 moved: GitHub's body is cdcc073f4531, --expect "000000000000" (read it again with --dry-run)`},
		{"no expect", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) { r.Expect = "" },
			`refused: PRRC_00000003 moved: GitHub's body is cdcc073f4531, --expect "" (read it again with --dry-run)`},
		{"another repo", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) { r.Repo = "example/other" },
			"refused: PRRC_00000003 is in example/repo, not example/other"},
		{"comment_edit not granted", func(_ *fakeComment, w *model.Critic, _ *commentEditReq) {
			w.GHWrite.Fields = []string{"thread_reply", "thread_resolve", "labels", "draft", "issue_create", "comment_create"}
		}, "refused: field comment_edit is not in the trusted write_fields"},
		{"a read-only critic", func(_ *fakeComment, w *model.Critic, _ *commentEditReq) { w.Write = false },
			"refused: this critic is read-only: write comes from ebac's trusted config (authority.write), not from its form"},
		{"no ours login", func(_ *fakeComment, w *model.Critic, _ *commentEditReq) { w.GHWrite.Ours = "" },
			"refused: critic gh-cache's trusted scope names no ours login: no comment is ours"},
		{"a PR of a read-only author", func(c *fakeComment, _ *model.Critic, _ *commentEditReq) { c.onAuthor = "someone" },
			"refused: example/repo#1001: author someone is read-only in the trusted scope"},
		{"an issue no scope PR fixes", func(c *fakeComment, _ *model.Critic, _ *commentEditReq) { c.review, c.onPR = false, false },
			"refused: issue example/repo#1001 is fixed by no PR in the trusted scope"},
		{"not a comment node id", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) { r.ID = "PR_00000009" },
			`refused: --comment "PR_00000009" is not a comment node id (IC_... or PRRC_...)`},
		{"an empty body", func(_ *fakeComment, _ *model.Critic, r *commentEditReq) { r.Body = " \n" },
			"refused: an empty body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := ourReviewComment(), ghCacheCritic()
			r := commentEditReq{Repo: "example/repo", ID: ourID, Body: scrubBody, Expect: leakHash}
			tc.edit(c, w, &r)
			gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: c}}
			out, err := commentEdit(context.Background(), gh, w, r)
			if err == nil {
				t.Fatalf("not refused: %q", out)
			}
			if err.Error() != tc.reason {
				t.Fatalf("err = %q\nwant  %q", err, tc.reason)
			}
			if len(gh.mutations) != 0 {
				t.Fatalf("a refusal wrote: %q", gh.mutations)
			}
			if c.body != leakBody {
				t.Fatalf("body moved: %q", c.body)
			}
		})
	}
}

// A comment GitHub no longer has is refused before any write.
func TestAGoneCommentIsRefused(t *testing.T) {
	gh := &fakeCommentGH{comments: map[string]*fakeComment{}}
	_, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, Body: scrubBody, Expect: leakHash})
	if err == nil || err.Error() != "PRRC_00000003 is not a comment GitHub has (deleted, or another kind of node)" || len(gh.mutations) != 0 {
		t.Fatalf("err = %v, mutations %q", err, gh.mutations)
	}
}

// A dry run prints the current body's --expect value and writes nothing.
func TestADryRunPrintsTheExpectAndWritesNothing(t *testing.T) {
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: ourReviewComment()}}
	out, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := "PRRC_00000003 on example/repo#1001: current body --expect cdcc073f4531 (95 B); nothing written"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if len(gh.mutations) != 0 {
		t.Fatalf("dry run wrote: %q", gh.mutations)
	}
}

// A dry run is still refused on a foreign author: it never hands out an --expect for a text we may not edit.
func TestADryRunOfAForeignCommentIsRefused(t *testing.T) {
	c := ourReviewComment()
	c.author = "someone"
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: c}}
	if _, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, DryRun: true}); err == nil {
		t.Fatal("dry run of a foreign comment not refused")
	}
}

// GitHub already holding the body: nothing written, said so.
func TestAnEditGitHubAlreadyHoldsWritesNothing(t *testing.T) {
	c := ourReviewComment()
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: c}}
	out, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, Body: leakBody, Expect: leakHash})
	if err != nil || out != "PRRC_00000003 on example/repo#1001: GitHub already holds the body; nothing written" || len(gh.mutations) != 0 {
		t.Fatalf("out %q err %v mutations %q", out, err, gh.mutations)
	}
}

// A failed mutation is an error naming the comment.
func TestAFailedEditIsAnError(t *testing.T) {
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: ourReviewComment()}, failEdit: fmt.Errorf("FORBIDDEN Resource not accessible")}
	_, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, Body: scrubBody, Expect: leakHash})
	if err == nil || err.Error() != "the edit of PRRC_00000003 failed: FORBIDDEN Resource not accessible" {
		t.Fatalf("err = %v", err)
	}
}

// A read-back that does not hold the body sent is an error, even though the mutation returned ok.
func TestAReadBackThatDoesNotHoldTheBodyIsAnError(t *testing.T) {
	gh := &fakeCommentGH{comments: map[string]*fakeComment{ourID: ourReviewComment()}, dropEdit: true}
	_, err := commentEdit(context.Background(), gh, ghCacheCritic(), commentEditReq{Repo: "example/repo", ID: ourID, Body: scrubBody, Expect: leakHash})
	want := "edited PRRC_00000003, but the read-back holds cdcc073f4531, not the body sent (" + bodyHash(scrubBody) + ")"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant  %s", err, want)
	}
}
