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

// markSel is a PR's change marker: head, updatedAt (moves on body, title,
// label, comment and review edits), state and the check rollup.
const markSel = `number updatedAt headRefOid state isDraft commits(last:1){nodes{commit{statusCheckRollup{state}}}}`

// deepSel is everything gh and parsed are built from.
const deepSel = `number url title body state isDraft mergeable mergeStateStatus headRefOid headRefName baseRefName additions deletions updatedAt createdAt
 author{login} labels(first:20){nodes{name}}
 ready: timelineItems(itemTypes:[READY_FOR_REVIEW_EVENT], last:1){nodes{... on ReadyForReviewEvent{createdAt}}}
 commits(last:1){nodes{commit{tree{oid} statusCheckRollup{state contexts(first:60){nodes{__typename ... on CheckRun{name conclusion status detailsUrl completedAt} ... on StatusContext{context state targetUrl}}}}}}}
 closingIssuesReferences(first:10){nodes{number}}
 reviews(last:30){nodes{author{login} state submittedAt body commit{oid tree{oid}}}}
 comments(last:30){nodes{author{login} body createdAt url}}
 reviewThreads(first:50){totalCount nodes{id path isResolved comments(last:1){totalCount nodes{author{login}}}}}`

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

// Ref is one PR of one repo.
type Ref struct {
	Repo string
	N    int
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
	repos, m := byRepo(prs)
	for i, repo := range repos {
		o, n := splitRepo(repo)
		fmt.Fprintf(&b, ` r%d: repository(owner:"%s",name:"%s"){`, i, o, n)
		for _, num := range m[repo] {
			fmt.Fprintf(&b, ` p%d: pullRequest(number:%d){%s}`, num, num, markSel)
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

// DeepQuery reads the given PRs (at most DeepBatch) in full.
func DeepQuery(prs []Ref) string {
	var b strings.Builder
	b.WriteString("{")
	repos, m := byRepo(prs)
	for i, repo := range repos {
		o, n := splitRepo(repo)
		fmt.Fprintf(&b, ` r%d: repository(owner:"%s",name:"%s"){`, i, o, n)
		for _, num := range m[repo] {
			fmt.Fprintf(&b, ` p%d: pullRequest(number:%d){...D}`, num, num)
		}
		b.WriteString("}")
	}
	b.WriteString("} fragment D on PullRequest {")
	b.WriteString(deepSel)
	b.WriteString("}")
	return b.String()
}

// Repos returns the repository alias -> repo map MarksQuery and DeepQuery used.
func repoAliases(prs []Ref) map[string]string {
	repos, _ := byRepo(prs)
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
	Commits   struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// Mark is a PR's change marker: head, updatedAt, state, draft and rollup.
type Mark struct {
	Head      string `json:"head"`
	UpdatedAt string `json:"updated_at"`
	State     string `json:"state"`
	Draft     bool   `json:"draft,omitempty"`
	Rollup    string `json:"rollup,omitempty"`
}

// Mark flattens the node.
func (m MarkNode) Mark() Mark {
	r := ""
	if len(m.Commits.Nodes) > 0 && m.Commits.Nodes[0].Commit.Rollup != nil {
		r = m.Commits.Nodes[0].Commit.Rollup.State
	}
	return Mark{Head: m.Head, UpdatedAt: m.UpdatedAt, State: m.State, Draft: m.Draft, Rollup: r}
}

// Comment is an issue comment.
type Comment struct {
	Author    *Login `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
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
		Nodes []struct {
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
						Nodes []Context `json:"nodes"`
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
		Nodes []Review `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		Nodes []Comment `json:"nodes"`
	} `json:"comments"`
	Threads struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			ID       string `json:"id"`
			Path     string `json:"path"`
			Resolved bool   `json:"isResolved"`
			Comments struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					Author *Login `json:"author"`
				} `json:"nodes"`
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
	r := ""
	if len(p.Commits.Nodes) > 0 && p.Commits.Nodes[0].Commit.Rollup != nil {
		r = p.Commits.Nodes[0].Commit.Rollup.State
	}
	return Mark{Head: p.Head, UpdatedAt: p.UpdatedAt, State: p.State, Draft: p.Draft, Rollup: r}
}
