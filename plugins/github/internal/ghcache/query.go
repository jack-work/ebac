package ghcache

import (
	"fmt"
	"sort"
	"strings"
)

// The GraphQL this source sends. GraphQL points are charged per call (min 1),
// so the marks read is ONE aliased call for every search and every due PR,
// and the deep read batches DeepBatch PRs per call.

// DeepBatch bounds the PRs of one deep read (each carries up to 30 comments,
// 30 reviews and 50 threads; six measured 3 points with a search, 2026-11-20).
const DeepBatch = 10

// SearchFirst is how many PRs one search alias returns.
const SearchFirst = 100

// countsSel is the checks' cheap digest: how many check runs and status
// contexts are in each state (one pipeline flipping moves it while the
// rollup can stay the same).
const countsSel = `checkRunCountsByState{state count} statusContextCountsByState{state count}`

// markSel is a PR's change marker: head, updatedAt (moves on body, title,
// label, comment and review edits), state, draft, mergeability (moves on a
// base move with no head move, and when GitHub settles UNKNOWN) and the
// checks (rollup and counts by state).
const markSel = `number updatedAt headRefOid state isDraft mergeable mergeStateStatus commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:1){` + countsSel + `}}}}}`

// deepSel is everything gh and parsed are built from.
const deepSel = `id number url title body state isDraft mergeable mergeStateStatus headRefOid headRefName baseRefName additions deletions updatedAt createdAt
 author{login} labels(first:20){totalCount nodes{name}}
 ready: timelineItems(itemTypes:[READY_FOR_REVIEW_EVENT], last:1){nodes{... on ReadyForReviewEvent{createdAt}}}
 commits(last:1){nodes{commit{tree{oid} statusCheckRollup{state contexts(first:60){totalCount ` + countsSel + ` nodes{__typename ... on CheckRun{name conclusion status detailsUrl completedAt} ... on StatusContext{context state targetUrl}}}}}}}
 closingIssuesReferences(first:10){nodes{number}}
 reviews(last:30){totalCount nodes{author{login} state submittedAt body commit{oid tree{oid}}}}
 comments(last:30){totalCount nodes{` + commentSel + `}}
 reviewThreads(first:50){totalCount nodes{id path isResolved comments(last:3){totalCount nodes{` + commentSel + `}}}}`

// commentSel is one conversation or review-thread comment (#45: the bodies
// the in-place edits diff against).
const commentSel = `id author{login} body createdAt updatedAt url`

// ThreadLast is how many of each review thread's newest comments the deep
// read carries (comments(last:3) in deepSel; plus, from a fuller read,
// every comment of ours).
const ThreadLast = 3

// issueMarkSel is an issue's change marker (#45): updatedAt moves on title,
// body, label and comment changes; the comment count catches the rest.
const issueMarkSel = `number updatedAt state comments{totalCount}`

// issueDeepSel is everything an issue's gh and parsed are built from.
const issueDeepSel = `id number url title body state updatedAt createdAt author{login} labels(first:20){totalCount nodes{name}}
 comments(last:30){totalCount nodes{` + commentSel + `}}`

// Search is one search alias of the marks query: a rule's selector in a repo.
type Search struct {
	Alias string
	Repo  string
	Rule  int
	Query string // the search string, quoted terms inside
}

// Searches are the aliases a config asks for, in a stable order: one per
// (repo, rule, author) (GitHub search ANDs author: qualifiers), one per (repo,
// rule) for labels (label:a,b is an OR) and one for title tags (in:title with
// OR). Explicit PR numbers need no search.
func Searches(c *Config) []Search {
	var out []Search
	add := func(repo string, rule int, q string) {
		out = append(out, Search{Alias: fmt.Sprintf("s%d", len(out)), Repo: repo, Rule: rule,
			Query: fmt.Sprintf("repo:%s is:pr is:open %s", repo, q)})
	}
	for _, repo := range c.Repos {
		for i, r := range c.Rules {
			if r.Repo != "" && r.Repo != repo {
				continue
			}
			for _, a := range r.Author {
				add(repo, i, "author:"+a)
			}
			if len(r.Label) > 0 {
				q := make([]string, len(r.Label))
				for j, l := range r.Label {
					q[j] = `\"` + l + `\"`
				}
				add(repo, i, "label:"+strings.Join(q, ","))
			}
			if len(r.TitleTag) > 0 {
				q := make([]string, len(r.TitleTag))
				for j, t := range r.TitleTag {
					q[j] = `\"` + t + `\"`
				}
				add(repo, i, "in:title "+strings.Join(q, " OR "))
			}
		}
	}
	return out
}

// Ref is one PR, or with Issue one issue, of one repo.
type Ref struct {
	Repo  string
	N     int
	Issue bool
}

func refOf(e *Entity) Ref { return Ref{e.Repo, e.N, e.Kind == KindIssue} }

// split is refs' PR numbers and issue numbers, each in order.
func split(refs []Ref) (prs, issues []int) {
	for _, r := range refs {
		if r.Issue {
			issues = append(issues, r.N)
		} else {
			prs = append(prs, r.N)
		}
	}
	sort.Ints(prs)
	sort.Ints(issues)
	return prs, issues
}

// grouped is refs by repo, the repos sorted.
func grouped(refs []Ref) ([]string, map[string][]Ref) {
	m := map[string][]Ref{}
	for _, r := range refs {
		m[r.Repo] = append(m[r.Repo], r)
	}
	repos := make([]string, 0, len(m))
	for r := range m {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, m
}

func splitRepo(repo string) (string, string) {
	o, n, _ := strings.Cut(repo, "/")
	return o, n
}

// byRepo groups refs by repo, each group in number order, the repos sorted.
func byRepo(refs []Ref) ([]string, map[string][]int) {
	m := map[string][]int{}
	for _, r := range refs {
		m[r.Repo] = append(m[r.Repo], r.N)
	}
	repos := make([]string, 0, len(m))
	for r, ns := range m {
		sort.Ints(ns)
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, m
}

// MarksQuery is the round's one marks call: every search alias, and the marks
// of the given PRs (tracked ones the searches do not answer), under one
// repository alias per repo (r0, r1, ...; each PR p<n>).
func MarksQuery(searches []Search, prs []Ref) string {
	var b strings.Builder
	b.WriteString("{")
	for _, s := range searches {
		fmt.Fprintf(&b, ` %s: search(query:"%s", type:ISSUE, first:%d){issueCount nodes{... on PullRequest{%s}}}`, s.Alias, s.Query, SearchFirst, markSel)
	}
	repos, m := grouped(prs)
	for i, repo := range repos {
		o, n := splitRepo(repo)
		fmt.Fprintf(&b, ` r%d: repository(owner:"%s",name:"%s"){`, i, o, n)
		ps, is := split(m[repo])
		for _, num := range ps {
			fmt.Fprintf(&b, ` p%d: pullRequest(number:%d){%s}`, num, num, markSel)
		}
		for _, num := range is {
			fmt.Fprintf(&b, ` i%d: issue(number:%d){%s}`, num, num, issueMarkSel)
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

// DeepQuery reads the given PRs and issues (at most DeepBatch) in full.
func DeepQuery(refs []Ref) string {
	var b strings.Builder
	b.WriteString("{")
	repos, m := grouped(refs)
	anyPR, anyIssue := false, false
	for i, repo := range repos {
		o, n := splitRepo(repo)
		fmt.Fprintf(&b, ` r%d: repository(owner:"%s",name:"%s"){`, i, o, n)
		ps, is := split(m[repo])
		for _, num := range ps {
			fmt.Fprintf(&b, ` p%d: pullRequest(number:%d){...D}`, num, num)
			anyPR = true
		}
		for _, num := range is {
			fmt.Fprintf(&b, ` i%d: issue(number:%d){...I}`, num, num)
			anyIssue = true
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	if anyPR {
		b.WriteString(" fragment D on PullRequest {" + deepSel + "}")
	}
	if anyIssue {
		b.WriteString(" fragment I on Issue {" + issueDeepSel + "}")
	}
	return b.String()
}

// Repos returns the repository alias -> repo map MarksQuery and DeepQuery used.
func repoAliases(prs []Ref) map[string]string {
	repos, _ := grouped(prs)
	out := map[string]string{}
	for i, r := range repos {
		out[fmt.Sprintf("r%d", i)] = r
	}
	return out
}

// Login is a GraphQL actor.
type Login struct {
	Login string `json:"login"`
}

// MarkNode is a PR's change marker as the marks query answers it.
type MarkNode struct {
	Number    int    `json:"number"`
	UpdatedAt string `json:"updatedAt"`
	Head      string `json:"headRefOid"`
	State     string `json:"state"`
	Draft     bool   `json:"isDraft"`
	Mergeable string `json:"mergeable"`
	MergeSt   string `json:"mergeStateStatus"`
	Commits   struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State    string `json:"state"`
					Contexts Counts `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// Counts are the checks' counts by state (countsSel).
type Counts struct {
	Runs     []StateCount `json:"checkRunCountsByState"`
	Statuses []StateCount `json:"statusContextCountsByState"`
}

// StateCount is one state's count.
type StateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// Digest is the counts as one stable string of the non-zero states
// ("run:SUCCESS=4,status:PENDING=1"); "" when there are none.
func (c Counts) Digest() string {
	var parts []string
	for _, x := range []struct {
		kind string
		cs   []StateCount
	}{{"run", c.Runs}, {"status", c.Statuses}} {
		for _, s := range x.cs {
			if s.Count > 0 {
				parts = append(parts, fmt.Sprintf("%s:%s=%d", x.kind, s.State, s.Count))
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Mark is a PR's change marker: head, updatedAt, state, draft, rollup,
// mergeability and the checks digest. A deep read happens only when it moves
// (or a head moves, or a write is pending): GitHub may answer UNKNOWN
// mergeability for hours, and that costs nothing until it changes.
type Mark struct {
	Head       string `json:"head"`
	UpdatedAt  string `json:"updated_at"`
	State      string `json:"state"`
	Draft      bool   `json:"draft,omitempty"`
	Rollup     string `json:"rollup,omitempty"`
	Mergeable  string `json:"mergeable,omitempty"`
	MergeState string `json:"merge_state,omitempty"`
	Checks     string `json:"checks,omitempty"`
	Comments   int    `json:"comments,omitempty"` // an issue's comment count (#45)
}

// Mark flattens the node.
func (m MarkNode) Mark() Mark {
	r, d := "", ""
	if len(m.Commits.Nodes) > 0 && m.Commits.Nodes[0].Commit.Rollup != nil {
		r = m.Commits.Nodes[0].Commit.Rollup.State
		d = m.Commits.Nodes[0].Commit.Rollup.Contexts.Digest()
	}
	return Mark{Head: m.Head, UpdatedAt: m.UpdatedAt, State: m.State, Draft: m.Draft, Rollup: r, Mergeable: m.Mergeable, MergeState: m.MergeSt, Checks: d}
}

// Comment is an issue comment or a review-thread comment.
type Comment struct {
	ID        string `json:"id"`
	Author    *Login `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	URL       string `json:"url"`
}

// IssueMarkNode is an issue's change marker as the marks query answers it.
type IssueMarkNode struct {
	Number    int    `json:"number"`
	UpdatedAt string `json:"updatedAt"`
	State     string `json:"state"`
	Comments  struct {
		TotalCount int `json:"totalCount"`
	} `json:"comments"`
}

// Mark flattens the node.
func (m IssueMarkNode) Mark() Mark {
	return Mark{UpdatedAt: m.UpdatedAt, State: m.State, Comments: m.Comments.TotalCount}
}

// Issue is the deep read of one issue (#45).
type Issue struct {
	ID        string `json:"id"`
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	UpdatedAt string `json:"updatedAt"`
	CreatedAt string `json:"createdAt"`
	Author    *Login `json:"author"`
	Labels    struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Comments struct {
		TotalCount int       `json:"totalCount"`
		Nodes      []Comment `json:"nodes"`
	} `json:"comments"`
}

// Mark is the deep read's own marker.
func (is *Issue) Mark() Mark {
	return Mark{UpdatedAt: is.UpdatedAt, State: is.State, Comments: is.Comments.TotalCount}
}

// asPR is the issue as a PR with no PR-only parts: what the parsers and the
// sync read (they never look at what an issue lacks).
func (is *Issue) asPR() *PR {
	pr := &PR{ID: is.ID, Number: is.Number, URL: is.URL, Title: is.Title, Body: is.Body, State: is.State,
		UpdatedAt: is.UpdatedAt, CreatedAt: is.CreatedAt, Author: is.Author}
	pr.Labels.TotalCount = is.Labels.TotalCount
	for _, l := range is.Labels.Nodes {
		pr.Labels.Nodes = append(pr.Labels.Nodes, struct {
			Name string `json:"name"`
		}{l.Name})
	}
	pr.Comments.TotalCount, pr.Comments.Nodes = is.Comments.TotalCount, is.Comments.Nodes
	return pr
}

// Review is a review submission.
type Review struct {
	Author      *Login `json:"author"`
	State       string `json:"state"`
	SubmittedAt string `json:"submittedAt"`
	Body        string `json:"body"`
	Commit      *struct {
		Oid  string `json:"oid"`
		Tree *struct {
			Oid string `json:"oid"`
		} `json:"tree"`
	} `json:"commit"`
}

// Context is one check: a CheckRun or a StatusContext.
type Context struct {
	Typename    string `json:"__typename"`
	Name        string `json:"name"`
	Conclusion  string `json:"conclusion"`
	Status      string `json:"status"`
	DetailsURL  string `json:"detailsUrl"`
	CompletedAt string `json:"completedAt"`
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"targetUrl"`
}

// PR is the deep read of one pull request.
type PR struct {
	ID        string `json:"id"` // the node id the writes name
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Draft     bool   `json:"isDraft"`
	Mergeable string `json:"mergeable"`
	MergeSt   string `json:"mergeStateStatus"`
	Head      string `json:"headRefOid"`
	Branch    string `json:"headRefName"`
	Base      string `json:"baseRefName"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	UpdatedAt string `json:"updatedAt"`
	CreatedAt string `json:"createdAt"`
	Author    *Login `json:"author"`
	Labels    struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Ready struct {
		Nodes []struct {
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"ready"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Tree *struct {
					Oid string `json:"oid"`
				} `json:"tree"`
				Rollup *struct {
					State    string `json:"state"`
					Contexts struct {
						TotalCount int       `json:"totalCount"`
						Nodes      []Context `json:"nodes"`
						Counts
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Closing struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Reviews struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []Review `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		TotalCount int       `json:"totalCount"`
		Nodes      []Comment `json:"nodes"`
	} `json:"comments"`
	Threads struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			ID       string `json:"id"`
			Path     string `json:"path"`
			Resolved bool   `json:"isResolved"`
			Comments struct {
				TotalCount int       `json:"totalCount"`
				Nodes      []Comment `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

func login(l *Login) string {
	if l == nil {
		return ""
	}
	return l.Login
}

// Mark is the deep read's own marker (what the next marks read compares).
func (p *PR) Mark() Mark {
	r, d := "", ""
	if len(p.Commits.Nodes) > 0 && p.Commits.Nodes[0].Commit.Rollup != nil {
		r = p.Commits.Nodes[0].Commit.Rollup.State
		d = p.Commits.Nodes[0].Commit.Rollup.Contexts.Counts.Digest()
	}
	return Mark{Head: p.Head, UpdatedAt: p.UpdatedAt, State: p.State, Draft: p.Draft, Rollup: r, Mergeable: p.Mergeable, MergeState: p.MergeSt, Checks: d}
}
