package model

import (
	"fmt"
	"slices"
	"strings"
)

// GHWrite is a github-cache critic's write authority: the
// entities and fields its sync may write to GitHub. It comes only from
// ebac's trusted config (Authority.Scope, WriteFields); the critic's form,
// its config key (rules, maintainer) and the entity forms are seat-writable,
// so they choose what the cache READS and never widen what it writes.
type GHWrite struct {
	Scope  GHScope  `json:"scope"`
	Fields []string `json:"fields,omitempty"`
	// Ours is the account login whose texts are ours (trusted ours_login).
	Ours string `json:"ours_login,omitempty"`
}

// GHScope is the trusted write scope: a PR is in it when its repo is one of
// Repos, its author (case-insensitive) is one of Authors or the PR is named
// in PRs ("owner/name#n"), and its author is not one of ReadOnlyAuthors. An
// issue is in it only when IssuesFixedByScope and an in-scope PR fixes it.
type GHScope struct {
	Repos              []string `json:"repos"`
	Authors            []string `json:"authors,omitempty"`
	PRs                []string `json:"prs,omitempty"`
	ReadOnlyAuthors    []string `json:"read_only_authors,omitempty"`
	IssuesFixedByScope bool     `json:"issues_fixed_by_scope,omitempty"`
}

// The wave-1 write fields (DESIGN 2): thread
// replies, thread resolves, labels and draft. Title and body are wave 2.
const (
	FieldThreadReply   = "thread_reply"
	FieldThreadResolve = "thread_resolve"
	FieldLabels        = "labels"
	FieldDraft         = "draft"
)

// GHWriteFields is the closed set of write fields.
var GHWriteFields = []string{FieldThreadReply, FieldThreadResolve, FieldLabels, FieldDraft}

// GHEntity is what a write's authorization looks at: the entity's repo,
// kind (pr | issue), number and author, and for an issue the PRs that fix it
// (as "owner/name#n" with their authors).
type GHEntity struct {
	Repo    string
	Kind    string
	N       int
	Author  string
	FixedBy map[string]string // issue only: fixing PR -> its author
}

// AuthorizeGHWrite says why the github-cache critic w may not write field on
// e, or "" if it may. Every clause fails closed: no write authority, a
// field outside the trusted write_fields, a repo, author or PR outside the
// trusted scope, a read-only author.
func AuthorizeGHWrite(w *Critic, e GHEntity, field string) string {
	if !w.Write {
		return "this critic is read-only: write comes from ebac's trusted config (authority.write), not from its form"
	}
	g := w.GHWrite
	if g == nil {
		return "no trusted write scope (authority.scope): nothing is written"
	}
	if !slices.Contains(GHWriteFields, field) {
		return fmt.Sprintf("field %q is not a write field (%s)", field, strings.Join(GHWriteFields, ", "))
	}
	if !slices.Contains(g.Fields, field) {
		return fmt.Sprintf("field %s is not in the trusted write_fields", field)
	}
	switch e.Kind {
	case "pr":
		return g.Scope.prOut(e.Repo, e.N, e.Author)
	case "issue":
		if !slices.Contains(g.Scope.Repos, e.Repo) {
			return fmt.Sprintf("repo %s is outside the trusted scope", e.Repo)
		}
		if !g.Scope.IssuesFixedByScope {
			return fmt.Sprintf("issue %s#%d: issues are outside the trusted scope (issues_fixed_by_scope is off)", e.Repo, e.N)
		}
		for pr, author := range e.FixedBy {
			repo, n, ok := SplitRef(pr)
			if ok && g.Scope.prOut(repo, n, author) == "" {
				return ""
			}
		}
		return fmt.Sprintf("issue %s#%d is fixed by no PR in the trusted scope", e.Repo, e.N)
	}
	return fmt.Sprintf("entity kind %q is not pr or issue", e.Kind)
}

func (s GHScope) prOut(repo string, n int, author string) string {
	ref := fmt.Sprintf("%s#%d", repo, n)
	if !slices.Contains(s.Repos, repo) {
		return fmt.Sprintf("repo %s is outside the trusted scope", repo)
	}
	if author == "" {
		return fmt.Sprintf("%s: author unknown: refused", ref)
	}
	if slices.ContainsFunc(s.ReadOnlyAuthors, func(a string) bool { return strings.EqualFold(a, author) }) {
		return fmt.Sprintf("%s: author %s is read-only in the trusted scope", ref, author)
	}
	if slices.ContainsFunc(s.Authors, func(a string) bool { return strings.EqualFold(a, author) }) || slices.Contains(s.PRs, ref) {
		return ""
	}
	return fmt.Sprintf("%s (author %s) is outside the trusted scope", ref, author)
}

// SplitRef reads "owner/name#n".
func SplitRef(ref string) (repo string, n int, ok bool) {
	repo, num, found := strings.Cut(ref, "#")
	if !found || !strings.Contains(repo, "/") {
		return "", 0, false
	}
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil || n <= 0 || fmt.Sprint(n) != num {
		return "", 0, false
	}
	return repo, n, true
}

// ghWrite is the trusted write authority a carries, nil when it has none.
func (a Authority) ghWrite() *GHWrite {
	if a.Scope == nil && len(a.WriteFields) == 0 && a.OursLogin == "" {
		return nil
	}
	g := &GHWrite{Fields: slices.Clone(a.WriteFields), Ours: a.OursLogin}
	if a.Scope != nil {
		g.Scope = GHScope{Repos: slices.Clone(a.Scope.Repos), Authors: slices.Clone(a.Scope.Authors), PRs: slices.Clone(a.Scope.PRs),
			ReadOnlyAuthors: slices.Clone(a.Scope.ReadOnlyAuthors), IssuesFixedByScope: a.Scope.IssuesFixedByScope}
	}
	return g
}

// ApproveRef is the approve_prs entry that allows an approval of repo#n at
// head sha.
func ApproveRef(repo string, n int, sha string) string { return fmt.Sprintf("%s#%d@%s", repo, n, sha) }
