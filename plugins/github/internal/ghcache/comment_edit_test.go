package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// comment_edit (#64). Synthesized fixtures only: repo example/repo, logins "ours" / "someone", node ids
// IC_0000000n (conversation comments) and PRRC_0000000n (review comments).

type commentGH struct {
	t         *testing.T
	comments  map[string]*fakeComment // id -> GitHub's comment
	mutations []string
	reads     int
	failMut   error
	onRead    func() // moves GitHub between the deep read and the pre-write read
}

type fakeComment struct {
	body, author string
	review       bool
	number       int
}

var commentReadRe = regexp.MustCompile(`(c\d+): node\(id:"([^"]+)"\)`)

func (f *commentGH) GraphQLPartial(_ context.Context, q string) (json.RawMessage, []forge.GQLError, error) {
	f.reads++
	if f.onRead != nil {
		f.onRead()
		f.onRead = nil
	}
	out := map[string]any{}
	var errs []forge.GQLError
	for _, m := range commentReadRe.FindAllStringSubmatch(q, -1) {
		c := f.comments[m[2]]
		if c == nil {
			out[m[1]] = nil
			errs = append(errs, forge.GQLError{Type: "NOT_FOUND", Path: []string{m[1]}})
			continue
		}
		n := map[string]any{"__typename": "IssueComment", "body": c.body, "viewerDidAuthor": c.author == "ours", "issue": map[string]any{"number": c.number}}
		if c.review {
			n = map[string]any{"__typename": "PullRequestReviewComment", "body": c.body, "viewerDidAuthor": c.author == "ours", "pullRequest": map[string]any{"number": c.number}}
		}
		out[m[1]] = n
	}
	data, err := json.Marshal(out)
	return data, errs, err
}

func (f *commentGH) PullHeads(context.Context, string, []int) (map[int]string, error) {
	return nil, nil
}

func (f *commentGH) Mutate(_ context.Context, q string, vars map[string]string) (json.RawMessage, error) {
	f.mutations = append(f.mutations, mutationName(q)+" "+vars["c"]+" b="+vars["b"])
	if f.failMut != nil {
		return nil, f.failMut
	}
	c := f.comments[vars["c"]]
	if c == nil {
		f.t.Fatalf("an edit of a comment the fake does not have: %s", vars["c"])
	}
	c.body = vars["b"]
	return json.RawMessage(`{}`), nil
}

// commentRig is one PR (example/repo#7) with an issue comment and a review thread comment by ours, and a
// conversation comment by someone; the entity's base agrees with GitHub.
type commentRig struct {
	t      *testing.T
	gh     *commentGH
	e      *Entity
	fields []string
	logs   []string
}

func newCommentRig(t *testing.T) *commentRig {
	gh := &commentGH{t: t, comments: map[string]*fakeComment{
		"IC_00000001":   {body: "first take", author: "ours", number: 7},
		"IC_00000002":   {body: "their note", author: "someone", number: 7},
		"PRRC_00000003": {body: "tracked elsewhere", author: "ours", review: true, number: 7},
	}}
	r := &commentRig{t: t, gh: gh, fields: []string{model.FieldCommentEdit}}
	r.e = &Entity{Repo: "example/repo", N: 7}
	g := r.github()
	b := baseOf(&g)
	r.e.Base = &b
	return r
}

// github is GitHub's state as the deep read caches it.
func (r *commentRig) github() GH {
	g := GH{Number: 7, Author: "ours", Comments: map[string]CachedComment{}, Threads: map[string]Thread{"PRRT_00000001": {Path: "a.go", Comments: map[string]CachedComment{}}}}
	for id, c := range r.gh.comments {
		cc := CachedComment{Author: c.author, Body: c.body}
		if c.review {
			g.Threads["PRRT_00000001"].Comments[id] = cc
		} else {
			g.Comments[id] = cc
		}
	}
	return g
}

// round runs one sync of the entity with the form's gh edited by edit; it returns the gh written.
func (r *commentRig) round(edit func(f *GH)) GH {
	r.t.Helper()
	F := r.github()
	F.Comments = cloneComments(F.Comments)
	th := F.Threads["PRRT_00000001"]
	th.Comments = cloneComments(th.Comments)
	F.Threads = map[string]Thread{"PRRT_00000001": th}
	edit(&F)
	fv := readForm(map[string]json.RawMessage{"gh": mustJSON(F)})
	if fv.gh == nil {
		r.t.Fatalf("the form did not parse: %v", fv.bad)
	}
	s := &syncer{ctx: context.Background(), gh: r.gh, now: time.Unix(1794000000, 0), logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) },
		p: Pass{Config: &Config{}, Ours: "ours", Auth: func(e model.GHEntity, field string) string {
			w := &model.Critic{Write: true, GHWrite: &model.GHWrite{Scope: model.GHScope{Repos: []string{"example/repo"}, Authors: []string{"ours"}}, Fields: r.fields}}
			return model.AuthorizeGHWrite(w, e, field)
		}}}
	out, _ := s.entity("k", r.e, &PR{ID: "PR_00000007", Number: 7}, r.github(), fv)
	if r.e.Pend != nil {
		b := r.e.Pend.Base
		r.e.Base = &b
	}
	return out
}

func cloneComments(m map[string]CachedComment) map[string]CachedComment {
	out := map[string]CachedComment{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func setBody(f *GH, id, body string) {
	if c, ok := f.Comments[id]; ok {
		c.Body = body
		f.Comments[id] = c
		return
	}
	t := f.Threads["PRRT_00000001"]
	c := t.Comments[id]
	c.Body = body
	t.Comments[id] = c
}

func (r *commentRig) alarm(kind, id string) *Alarm {
	if a, ok := r.e.Alarms[kind+"."+commentField(id)]; ok {
		return &a
	}
	return nil
}

func TestOurReviewCommentEditedInPlaceLandsWithTheBaseRechecked(t *testing.T) {
	r := newCommentRig(t)
	out := r.round(func(f *GH) { setBody(f, "PRRC_00000003", "tracked in #1.") })
	if got := r.gh.comments["PRRC_00000003"].body; got != "tracked in #1." {
		t.Fatalf("GitHub's review comment = %q, want the form's edit; mutations %v, logs %v, alarms %v", got, r.gh.mutations, r.logs, r.e.Alarms)
	}
	if len(r.gh.mutations) != 1 || !strings.HasPrefix(r.gh.mutations[0], "updatePullRequestReviewComment PRRC_00000003 ") {
		t.Fatalf("mutations = %v, want one updatePullRequestReviewComment", r.gh.mutations)
	}
	if r.gh.reads != 1 {
		t.Fatalf("pre-write reads = %d, want 1 (the base re-check)", r.gh.reads)
	}
	if out.Threads["PRRT_00000001"].Comments["PRRC_00000003"].Body != "tracked in #1." || r.e.Base.Comments["PRRC_00000003"] != hash12("tracked in #1.") {
		t.Fatalf("the edit is not the new base: out %+v base %v", out.Threads["PRRT_00000001"].Comments, r.e.Base.Comments)
	}
	r.round(func(f *GH) { setBody(f, "PRRC_00000003", "tracked in #1.") })
	if len(r.gh.mutations) != 1 {
		t.Fatalf("a settled edit was written again: %v", r.gh.mutations)
	}
}

func TestOurConversationCommentEditUsesUpdateIssueComment(t *testing.T) {
	r := newCommentRig(t)
	r.round(func(f *GH) { setBody(f, "IC_00000001", "second take") })
	if len(r.gh.mutations) != 1 || r.gh.mutations[0] != "updateIssueComment IC_00000001 b=second take" {
		t.Fatalf("mutations = %v", r.gh.mutations)
	}
}

func TestAnotherAuthorsCommentIsRefusedAndNothingIsWritten(t *testing.T) {
	r := newCommentRig(t)
	out := r.round(func(f *GH) { setBody(f, "IC_00000002", "rewritten") })
	if len(r.gh.mutations) != 0 || r.gh.reads != 0 {
		t.Fatalf("another author's comment reached GitHub: mutations %v reads %d", r.gh.mutations, r.gh.reads)
	}
	if a := r.alarm(AlarmWriteRefused, "IC_00000002"); a == nil || !strings.Contains(a.Detail, "not ours") {
		t.Fatalf("write-refused alarm = %+v, want one naming another author", a)
	}
	if out.Comments["IC_00000002"].Body != "their note" {
		t.Fatalf("the form keeps the edit of another author's text: %q", out.Comments["IC_00000002"].Body)
	}
}

func TestGitHubSaysNotOursAtWriteTimeIsRefused(t *testing.T) {
	r := newCommentRig(t)
	r.gh.onRead = func() { r.gh.comments["IC_00000001"].author = "someone" } // the cache said ours; GitHub's viewerDidAuthor decides
	r.round(func(f *GH) { setBody(f, "IC_00000001", "second take") })
	if len(r.gh.mutations) != 0 {
		t.Fatalf("written although GitHub says another author: %v", r.gh.mutations)
	}
	if a := r.alarm(AlarmWriteRefused, "IC_00000001"); a == nil || !strings.Contains(a.Detail, "did not write") {
		t.Fatalf("alarm = %+v", a)
	}
}

func TestABodyTheDisclosureRulesRefuseIsNotWritten(t *testing.T) {
	r := newCommentRig(t)
	r.round(func(f *GH) { setBody(f, "IC_00000001", "see 00000001 aria") })
	if len(r.gh.mutations) != 0 || r.gh.reads != 0 {
		t.Fatalf("a refused body reached GitHub: %v", r.gh.mutations)
	}
	if r.alarm(AlarmWriteRefused, "IC_00000001") == nil || r.e.Conflicts[commentField("IC_00000001")] == nil {
		t.Fatalf("no write-refused alarm + conflict value: alarms %v conflicts %v", r.e.Alarms, r.e.Conflicts)
	}
}

func TestAnEditOnBothSidesIsAConflictAndGitHubsStands(t *testing.T) {
	r := newCommentRig(t)
	r.gh.comments["IC_00000001"].body = "edited on GitHub"
	out := r.round(func(f *GH) { setBody(f, "IC_00000001", "edited on the form") })
	if len(r.gh.mutations) != 0 {
		t.Fatalf("a conflicting edit was written: %v", r.gh.mutations)
	}
	if r.alarm(AlarmConflict, "IC_00000001") == nil || out.Comments["IC_00000001"].Body != "edited on GitHub" {
		t.Fatalf("conflict alarm %v, out %q", r.e.Alarms, out.Comments["IC_00000001"].Body)
	}
}

func TestGitHubMovingBetweenReadAndWriteIsStaleThenAConflict(t *testing.T) {
	r := newCommentRig(t)
	r.gh.onRead = func() { r.gh.comments["IC_00000001"].body = "moved meanwhile" }
	out := r.round(func(f *GH) { setBody(f, "IC_00000001", "second take") })
	if len(r.gh.mutations) != 0 || out.Comments["IC_00000001"].Body != "second take" {
		t.Fatalf("stale edit: mutations %v, form keeps %q", r.gh.mutations, out.Comments["IC_00000001"].Body)
	}
	if r.e.Tries[commentField("IC_00000001")] != 1 {
		t.Fatalf("tries = %v", r.e.Tries)
	}
}

func TestACommentEditOutsideTheTrustedWriteFieldsIsRefused(t *testing.T) {
	r := newCommentRig(t)
	r.fields = []string{model.FieldLabels}
	r.round(func(f *GH) { setBody(f, "IC_00000001", "second take") })
	if len(r.gh.mutations) != 0 {
		t.Fatalf("written outside write_fields: %v", r.gh.mutations)
	}
	if a := r.alarm(AlarmWriteRefused, "IC_00000001"); a == nil || !strings.Contains(a.Detail, "write_fields") {
		t.Fatalf("alarm = %+v", a)
	}
}

func TestACutCommentIsNotEdited(t *testing.T) {
	r := newCommentRig(t)
	r.round(func(f *GH) {
		c := f.Comments["IC_00000001"]
		c.Body, c.Cut = "second take", true
		f.Comments["IC_00000001"] = c
	})
	if len(r.gh.mutations) != 0 || r.alarm(AlarmWriteRefused, "IC_00000001") == nil {
		t.Fatalf("a cut comment was edited: %v %v", r.gh.mutations, r.e.Alarms)
	}
}

func TestACommentOfAnotherEntityIsRefusedAtWriteTime(t *testing.T) {
	r := newCommentRig(t)
	r.gh.comments["IC_00000001"].number = 8
	r.round(func(f *GH) { setBody(f, "IC_00000001", "second take") })
	if len(r.gh.mutations) != 0 || r.alarm(AlarmWriteRefused, "IC_00000001") == nil {
		t.Fatalf("a comment of #8 edited through #7: %v", r.gh.mutations)
	}
}

func TestCommentBodiesAreNotReadOnlyFields(t *testing.T) {
	g := GH{Comments: map[string]CachedComment{"IC_00000001": {Author: "ours", Body: "a"}}}
	h := g
	h.Comments = map[string]CachedComment{"IC_00000001": {Author: "ours", Body: "b"}}
	if roHash(&g) != roHash(&h) {
		t.Fatal("a comment body counts as read-only: an edit would be reverted")
	}
	h.Comments = map[string]CachedComment{"IC_00000001": {Author: "someone", Body: "a"}}
	if roHash(&g) == roHash(&h) {
		t.Fatal("the comment author is not read-only")
	}
}
