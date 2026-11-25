package model

import (
	"reflect"
	"strings"
	"testing"
)

func scopedCritic() *Critic {
	return &Critic{Source: SourceGHCache, Write: true, GHWrite: &GHWrite{
		Scope:  GHScope{Repos: []string{"acme/widget"}, Authors: []string{"Our-Author"}, PRs: []string{"acme/widget#40"}, ReadOnlyAuthors: []string{"their-author"}, IssuesFixedByScope: true},
		Fields: []string{FieldThreadReply, FieldThreadResolve, FieldLabels, FieldDraft}}, Maintainer: "@0000000a"}
}

// Every clause of the github-cache write gate, each row a different refusal (the exact text), with the controls
// that pass (our author any case, an explicitly named PR, an issue fixed by our PR). RED if any clause goes: its
// row is allowed instead of refused.
func TestAuthorizeGHWriteEveryClause(t *testing.T) {
	pr := func(repo string, n int, author string) GHEntity {
		return GHEntity{Repo: repo, Kind: "pr", N: n, Author: author}
	}
	readOnly := scopedCritic()
	readOnly.Write = false
	noScope := scopedCritic()
	noScope.GHWrite = nil
	fewer := scopedCritic()
	fewer.GHWrite.Fields = []string{FieldThreadReply}
	noIssues := scopedCritic()
	noIssues.GHWrite.Scope.IssuesFixedByScope = false
	for _, tc := range []struct {
		name  string
		w     *Critic
		e     GHEntity
		field string
		want  string
	}{
		{"our PR", scopedCritic(), pr("acme/widget", 7, "our-author"), FieldLabels, ""},
		{"our PR, author in another case", scopedCritic(), pr("acme/widget", 7, "OUR-AUTHOR"), FieldDraft, ""},
		{"a PR named in prs", scopedCritic(), pr("acme/widget", 40, "someone"), FieldLabels, ""},
		{"an issue our PR fixes", scopedCritic(), GHEntity{Repo: "acme/widget", Kind: "issue", N: 9, FixedBy: map[string]string{"acme/widget#7": "our-author"}}, FieldLabels, ""},
		{"read-only critic", readOnly, pr("acme/widget", 7, "our-author"), FieldLabels, "this critic is read-only: write comes from ebac's trusted config (authority.write), not from its form"},
		{"no trusted scope", noScope, pr("acme/widget", 7, "our-author"), FieldLabels, "no trusted write scope (authority.scope): nothing is written"},
		{"unknown field", scopedCritic(), pr("acme/widget", 7, "our-author"), "title", `field "title" is not a write field (thread_reply, thread_resolve, labels, draft, issue_create, comment_create, comment_edit)`},
		{"field not in write_fields", fewer, pr("acme/widget", 7, "our-author"), FieldLabels, "field labels is not in the trusted write_fields"},
		{"another repo", scopedCritic(), pr("acme/other", 7, "our-author"), FieldLabels, "repo acme/other is outside the trusted scope"},
		{"another author", scopedCritic(), pr("acme/widget", 8, "stranger"), FieldLabels, "acme/widget#8 (author stranger) is outside the trusted scope"},
		{"read-only author wins over prs", scopedCritic(), pr("acme/widget", 40, "Their-Author"), FieldLabels, "acme/widget#40: author Their-Author is read-only in the trusted scope"},
		{"author unknown", scopedCritic(), pr("acme/widget", 40, ""), FieldLabels, "acme/widget#40: author unknown: refused"},
		{"issue, issues off", noIssues, GHEntity{Repo: "acme/widget", Kind: "issue", N: 9, FixedBy: map[string]string{"acme/widget#7": "our-author"}}, FieldLabels, "issue acme/widget#9: issues are outside the trusted scope (issues_fixed_by_scope is off)"},
		{"issue fixed only by their PR", scopedCritic(), GHEntity{Repo: "acme/widget", Kind: "issue", N: 9, FixedBy: map[string]string{"acme/widget#8": "their-author"}}, FieldLabels, "issue acme/widget#9 is fixed by no PR in the trusted scope"},
		{"issue fixed by nothing", scopedCritic(), GHEntity{Repo: "acme/widget", Kind: "issue", N: 9}, FieldLabels, "issue acme/widget#9 is fixed by no PR in the trusted scope"},
		{"issue in another repo", scopedCritic(), GHEntity{Repo: "acme/other", Kind: "issue", N: 9, FixedBy: map[string]string{"acme/widget#7": "our-author"}}, FieldLabels, "repo acme/other is outside the trusted scope"},
		{"unknown kind", scopedCritic(), GHEntity{Repo: "acme/widget", Kind: "run", N: 1}, FieldLabels, `entity kind "run" is not pr or issue`},
	} {
		if got := AuthorizeGHWrite(tc.w, tc.e, tc.field); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A form-held gh_write or approve_prs is replaced by the trusted one and named; the trusted values round-trip.
// RED if either is read from the form again.
func TestAuthorityGHWriteAndApprovePRsComeFromTrustedOnly(t *testing.T) {
	trusted := Authority{Source: SourceGHCache, Mode: ModeReview, Write: true,
		Scope:       &GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}},
		WriteFields: []string{FieldLabels}, Maintainer: "@0000000a", ApprovePRs: []string{"acme/widget#1@abc"}, AutomatedMarkers: []string{"[synthetic bot verdict]"}}
	form := &Critic{Source: SourceGHCache, Mode: ModeReview, Write: true, ApprovePRs: []string{"acme/widget#2@evil"},
		Maintainer: "@evil", AutomatedMarkers: []string{"LGTM"},
		GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author", "stranger"}}, Fields: GHWriteFields}}
	if got := strings.Join(trusted.Apply(form), ","); got != "gh_write,maintainer,approve_prs,automated_markers" {
		t.Fatalf("ignored %q, want gh_write,maintainer,approve_prs,automated_markers", got)
	}
	want := &GHWrite{Scope: GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}}, Fields: []string{FieldLabels}}
	if !reflect.DeepEqual(form.GHWrite, want) || !reflect.DeepEqual(form.ApprovePRs, []string{"acme/widget#1@abc"}) || form.Maintainer != "@0000000a" ||
		!reflect.DeepEqual(form.AutomatedMarkers, []string{"[synthetic bot verdict]"}) {
		t.Fatalf("after apply %+v %v %q %v", form.GHWrite, form.ApprovePRs, form.Maintainer, form.AutomatedMarkers)
	}
	if !reflect.DeepEqual(AuthorityOf(form), trusted) {
		t.Fatalf("round trip %+v, want %+v", AuthorityOf(form), trusted)
	}
	if again := trusted.Apply(form); len(again) != 0 {
		t.Fatalf("equal values flagged: %v", again)
	}
	// A trusted entry with no write authority clears a form-held one (nil, not an empty scope).
	bare := &Critic{GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"acme/widget"}}}}
	if got := strings.Join((Authority{}).Apply(bare), ","); got != "gh_write" || bare.GHWrite != nil {
		t.Fatalf("bare: %q %+v", got, bare.GHWrite)
	}
	if c := CreationAuthority(form); c.Scope != nil || c.WriteFields != nil || c.Maintainer != "" || c.ApprovePRs != nil || c.AutomatedMarkers != nil {
		t.Fatalf("creation records write authority: %+v", c)
	}
}

// The ref grammar: owner/name#n with a positive decimal n and nothing else. Adversarial rows.
func TestSplitRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		repo string
		n    int
		ok   bool
	}{
		{"acme/widget#7", "acme/widget", 7, true},
		{"acme/widget#07", "", 0, false},
		{"acme/widget#0", "", 0, false},
		{"acme/widget#-1", "", 0, false},
		{"acme/widget#7x", "", 0, false},
		{"acme/widget#", "", 0, false},
		{"widget#7", "", 0, false},
		{"", "", 0, false},
		{"acme/widget#7#8", "", 0, false},
	} {
		repo, n, ok := SplitRef(tc.in)
		if repo != tc.repo || n != tc.n || ok != tc.ok {
			t.Errorf("%q: got %q %d %v", tc.in, repo, n, ok)
		}
	}
}

// A2: an issue not created yet is in scope only by its repo, only for
// issue_create, and only when the scope names the ours login.
func TestANewIssueIsAuthorizedByRepoAndOursOnly(t *testing.T) {
	w := &Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Ours: "ours", Fields: []string{FieldIssueCreate, FieldCommentCreate}}}
	e := GHEntity{Repo: "example/repo", Kind: KindNewIssue}
	if why := AuthorizeGHWrite(w, e, FieldIssueCreate); why != "" {
		t.Fatalf("in scope: %s", why)
	}
	for name, c := range map[string]struct {
		w     *Critic
		e     GHEntity
		field string
		want  string
	}{
		"other repo":      {w, GHEntity{Repo: "example/other", Kind: KindNewIssue}, FieldIssueCreate, "repo example/other is outside the trusted scope"},
		"no ours":         {&Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Fields: []string{FieldIssueCreate}}}, e, FieldIssueCreate, "names no ours login"},
		"field not given": {&Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Ours: "ours", Fields: []string{FieldLabels}}}, e, FieldIssueCreate, "not in the trusted write_fields"},
		"wrong field":     {w, e, FieldCommentCreate, "does not apply to an issue not created yet"},
		"read-only":       {&Critic{GHWrite: w.GHWrite}, e, FieldIssueCreate, "read-only"},
	} {
		if why := AuthorizeGHWrite(c.w, c.e, c.field); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want it to say %q", name, why, c.want)
		}
	}
}

// A comment may be created on an issue the ours login authored (in a scope
// repo), and on no other issue unless issues_fixed_by_scope admits it.
func TestACommentOnOurOwnIssueIsInScopeAndOnlyThat(t *testing.T) {
	w := &Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Ours: "ours", Fields: []string{FieldCommentCreate, FieldLabels}}}
	ours := GHEntity{Repo: "example/repo", Kind: "issue", N: 5, Author: "Ours"}
	if why := AuthorizeGHWrite(w, ours, FieldCommentCreate); why != "" {
		t.Fatalf("a comment on our own issue: %s", why)
	}
	for name, c := range map[string]struct {
		e     GHEntity
		field string
		want  string
	}{
		"another author":    {GHEntity{Repo: "example/repo", Kind: "issue", N: 5, Author: "someone"}, FieldCommentCreate, "issues_fixed_by_scope is off"},
		"other repo":        {GHEntity{Repo: "example/other", Kind: "issue", N: 5, Author: "ours"}, FieldCommentCreate, "outside the trusted scope"},
		"labels on ours":    {ours, FieldLabels, "issues_fixed_by_scope is off"},
		"no author on file": {GHEntity{Repo: "example/repo", Kind: "issue", N: 5}, FieldCommentCreate, "issues_fixed_by_scope is off"},
	} {
		if why := AuthorizeGHWrite(w, c.e, c.field); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want %q", name, why, c.want)
		}
	}
	edit := &Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Ours: "ours", Fields: []string{FieldCommentEdit}}}
	if why := AuthorizeGHWrite(edit, ours, FieldCommentEdit); why != "" {
		t.Fatalf("an edit of our comment on our own issue: %s", why)
	}
	if why := AuthorizeGHWrite(edit, GHEntity{Repo: "example/repo", Kind: "issue", N: 5, Author: "someone"}, FieldCommentEdit); why != "issue example/repo#5: issues are outside the trusted scope (issues_fixed_by_scope is off)" {
		t.Fatalf("an edit on another author's issue: %q", why)
	}
	noOurs := &Critic{Write: true, GHWrite: &GHWrite{Scope: GHScope{Repos: []string{"example/repo"}}, Fields: []string{FieldCommentCreate}}}
	if why := AuthorizeGHWrite(noOurs, GHEntity{Repo: "example/repo", Kind: "issue", N: 5, Author: ""}, FieldCommentCreate); why == "" {
		t.Fatal("an empty ours login matched an empty author")
	}
}
