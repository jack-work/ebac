package scout

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/mergecheck"
)

// prFields is everything a pass reads about one PR, in one GraphQL selection:
// the entry's facts, the merge checklist's facts, the relevance filter's
// labels and first page of files, and the body's issue links. The architect
// and automaton verdicts are PR comments: a new one bumps the PR's updatedAt,
// so the incremental search sees it without polling.
const prFields = `number title url state isDraft mergeable mergeStateStatus headRefOid baseRefName
updatedAt mergedAt closedAt author{login} body labels(first:20){nodes{name}}
files(first:100){totalCount pageInfo{hasNextPage endCursor} nodes{path}}
closingIssuesReferences(first:10){nodes{number}}
latestOpinionatedReviews(first:20){nodes{author{login} state commit{oid}}}
reviewThreads(first:100){nodes{isResolved}}
comments(last:30){nodes{author{login} createdAt body}}
commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:60){nodes{
... on CheckRun{name conclusion status detailsUrl} ... on StatusContext{context state targetUrl}}}}}}}`

// issueFields is everything a pass reads about one issue, including who made
// each kind of change (S5): the last body editor, the last label and state
// events' actors, and the recent comments' authors.
const issueFields = `number title url state updatedAt closedAt author{login} labels(first:20){nodes{name}}
body lastEditedAt editor{__typename login}
comments(last:50){nodes{author{__typename login} createdAt}}
timelineItems(last:20, itemTypes:[LABELED_EVENT, UNLABELED_EVENT, CLOSED_EVENT, REOPENED_EVENT]){nodes{__typename
... on LabeledEvent{createdAt actor{__typename login}} ... on UnlabeledEvent{createdAt actor{__typename login}}
... on ClosedEvent{createdAt actor{__typename login}} ... on ReopenedEvent{createdAt actor{__typename login}}}}`

// searchQuery is ONE GraphQL call: one aliased search per author (a<i>), each
// for the author's PRs and issues in repo updated at or after the cursor
// (every state, so merges and closes are seen), or, with no cursor, every
// open one. after holds the page cursor of an alias still paging; a nil
// after map is the first page of every alias, and an alias absent from a
// non-nil map is done.
func searchQuery(repo string, authors []string, cursor string, after map[int]string) string {
	var b strings.Builder
	b.WriteString("{")
	for i, a := range authors {
		pg := ""
		if after != nil {
			c, ok := after[i]
			if !ok {
				continue
			}
			pg = fmt.Sprintf(`, after:%q`, c)
		}
		q := fmt.Sprintf("repo:%s author:%s", repo, a)
		if cursor == "" {
			q += " is:open"
		} else {
			q += " updated:>=" + cursor
		}
		fmt.Fprintf(&b, `a%d: search(type:ISSUE, first:%d%s, query:%q){pageInfo{hasNextPage endCursor} nodes{__typename ... on PullRequest{%s} ... on Issue{%s}}} `,
			i, Batch, pg, q, prFields, issueFields)
	}
	b.WriteString("}")
	return b.String()
}

// batchQuery reads named PRs (aliases p<n>) and issue-or-PR numbers (aliases
// i<n>) in one query, at most Batch in all.
func batchQuery(repo string, prs, issues []int) string {
	owner, name, _ := strings.Cut(repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "{repository(owner:%q, name:%q){", owner, name)
	for _, n := range prs {
		fmt.Fprintf(&b, "p%d: pullRequest(number:%d){%s} ", n, n, prFields)
	}
	for _, n := range issues {
		fmt.Fprintf(&b, "i%d: issueOrPullRequest(number:%d){__typename ... on Issue{%s}} ", n, n, issueFields)
	}
	b.WriteString("}}")
	return b.String()
}

// filesQuery is one more page of a PR's files (a PR changing more than the
// first page carries).
func filesQuery(repo string, n int, after string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return fmt.Sprintf(`{repository(owner:%q, name:%q){pullRequest(number:%d){files(first:100, after:%q){pageInfo{hasNextPage endCursor} nodes{path}}}}}`,
		owner, name, n, after)
}

type login struct {
	Typename string `json:"__typename,omitempty"`
	Login    string `json:"login"`
}

// bot says whether an actor is automation: a Bot, or a login ending [bot].
func (l *login) bot() bool {
	return l != nil && (l.Typename == "Bot" || strings.HasSuffix(l.Login, "[bot]"))
}

type labels struct {
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

func (l labels) names() []string {
	out := []string{}
	for _, n := range l.Nodes {
		out = append(out, n.Name)
	}
	return out
}

type filesPage struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		Path string `json:"path"`
	} `json:"nodes"`
}

// ghNode is the GraphQL shape of prFields and issueFields (a search node is
// either).
type ghNode struct {
	Typename         string    `json:"__typename"`
	Number           int       `json:"number"`
	Title            string    `json:"title"`
	URL              string    `json:"url"`
	State            string    `json:"state"`
	IsDraft          bool      `json:"isDraft"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	HeadRefOid       string    `json:"headRefOid"`
	BaseRefName      string    `json:"baseRefName"`
	UpdatedAt        string    `json:"updatedAt"`
	MergedAt         string    `json:"mergedAt"`
	ClosedAt         string    `json:"closedAt"`
	Author           *login    `json:"author"`
	Body             string    `json:"body"`
	Labels           labels    `json:"labels"`
	Files            filesPage `json:"files"`
	Closing          struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Reviews struct {
		Nodes []struct {
			Author *login `json:"author"`
			State  string `json:"state"`
			Commit *struct {
				Oid string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	Threads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Comments struct {
		Nodes []struct {
			Author    *login `json:"author"`
			CreatedAt string `json:"createdAt"`
			Body      string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
	LastEditedAt string `json:"lastEditedAt"`
	Editor       *login `json:"editor"`
	Timeline     struct {
		Nodes []struct {
			Typename  string `json:"__typename"`
			CreatedAt string `json:"createdAt"`
			Actor     *login `json:"actor"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Status     string `json:"status"`
							DetailsURL string `json:"detailsUrl"`
							Context    string `json:"context"`
							State      string `json:"state"`
							TargetURL  string `json:"targetUrl"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (n ghNode) isPR() bool {
	return n.Typename == "PullRequest" || n.HeadRefOid != "" || n.Typename == "" && n.BaseRefName != ""
}

func (n ghNode) author() string {
	if n.Author == nil {
		return ""
	}
	return n.Author.Login
}

// PR is one PR's facts as a pass read them.
type PR struct {
	Number    int
	Author    string
	Title     string
	Base      string
	Updated   string
	MergedAt  string
	ClosedAt  string
	Checks    string // rollup state
	Labels    []string
	Files     []string
	FilesMore string // the files page cursor when more pages follow
	Links     []int  // issue numbers the body or the closing references name
	Facts     mergecheck.Facts
	Arch      *mergecheck.Arch
	Automaton *Verdict
}

// Issue is one issue's facts as a pass read them.
type Issue struct {
	Number   int
	Author   string
	Title    string
	URL      string
	State    string // OPEN, CLOSED
	Updated  string
	ClosedAt string
	Labels   []string
	Body     string
	// Who made each kind of change last (S5): the body's last editor (its
	// author when never edited), the last label and state events' actors.
	BodyBy, LabelsBy, StateBy *login
	Comments                  []Comment // the recent comments, oldest first
}

// Comment is one issue comment's author and time.
type Comment struct {
	By *login
	At string
}

var buildRE = regexp.MustCompile(`buildId=(\d+)`)

// The architect-review[bot] verdict comment and its hold marker (minister-control graph.py, same patterns), and the
// Automaton's verdict marker (<!-- automaton:<pr>:<head8> verdict:<v> -->; its queue acks carry automaton-ack and
// no verdict).
var (
	archHeadRE    = regexp.MustCompile("Architect review\\s*[\u2014-]+\\s*PR #(\\d+)\\s*@\\s*`([0-9a-f]{7,40})`")
	archVerdictRE = regexp.MustCompile(`(?m)^\*\*([A-Z_]+)\*\*\s*$`)
	automatonRE   = regexp.MustCompile(`<!-- automaton:(\d+):([0-9a-f]{7,40}) verdict:([A-Za-z_]+) -->`)
)

const archBot = "architect-review[bot]"

// linkRE finds issue links in a PR body: #<n> (four or more digits: "step #1"
// is not a link) and .../<repo>/issues/<n> URLs.
var linkRE = regexp.MustCompile(`(?:^|[^\w&/#])#(\d{4,})\b|/issues/(\d+)\b`)

func links(n ghNode) []int {
	seen := map[int]bool{n.Number: true}
	var out []int
	add := func(k int) {
		if k > 0 && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, c := range n.Closing.Nodes {
		add(c.Number)
	}
	for _, m := range linkRE.FindAllStringSubmatch(n.Body, -1) {
		s := m[1]
		if s == "" {
			s = m[2]
		}
		var k int
		fmt.Sscan(s, &k)
		add(k)
	}
	return out
}

func toIssue(g ghNode) Issue {
	is := Issue{Number: g.Number, Author: g.author(), Title: g.Title, URL: g.URL, State: g.State, Updated: g.UpdatedAt,
		ClosedAt: g.ClosedAt, Labels: g.Labels.names(), Body: g.Body, BodyBy: g.Author}
	if g.LastEditedAt != "" && g.Editor != nil {
		is.BodyBy = g.Editor
	}
	for _, e := range g.Timeline.Nodes { // oldest first: the last one wins
		switch e.Typename {
		case "LabeledEvent", "UnlabeledEvent":
			is.LabelsBy = e.Actor
		case "ClosedEvent", "ReopenedEvent":
			is.StateBy = e.Actor
		}
	}
	for _, c := range g.Comments.Nodes {
		is.Comments = append(is.Comments, Comment{By: c.Author, At: c.CreatedAt})
	}
	return is
}

func toPR(g ghNode) PR {
	p := PR{Number: g.Number, Author: g.author(), Title: g.Title, Base: g.BaseRefName, Updated: g.UpdatedAt, MergedAt: g.MergedAt,
		ClosedAt: g.ClosedAt, Labels: g.Labels.names(), Links: links(g)}
	for _, f := range g.Files.Nodes {
		p.Files = append(p.Files, f.Path)
	}
	if g.Files.PageInfo.HasNextPage {
		p.FilesMore = g.Files.PageInfo.EndCursor
	}
	f := mergecheck.Facts{Oid: g.HeadRefOid, State: g.State, Draft: g.IsDraft, Heavy: true, Mergeable: g.Mergeable,
		MergeState: g.MergeStateStatus, Contexts: []mergecheck.Context{}, Reviews: []mergecheck.Review{}}
	if f.Mergeable == "" {
		f.Mergeable = "UNKNOWN"
	}
	for _, t := range g.Threads.Nodes {
		if !t.IsResolved {
			f.Unresolved++
		}
	}
	for _, r := range g.Reviews.Nodes {
		oid, who := "", ""
		if r.Commit != nil {
			oid = r.Commit.Oid
		}
		if r.Author != nil {
			who = r.Author.Login
		}
		f.Reviews = append(f.Reviews, mergecheck.Review{Login: who, State: r.State, Oid: oid})
	}
	if len(g.Commits.Nodes) > 0 && g.Commits.Nodes[0].Commit.Rollup != nil {
		roll := g.Commits.Nodes[0].Commit.Rollup
		p.Checks = roll.State
		for _, c := range roll.Contexts.Nodes {
			name := c.Name
			if name == "" {
				name = c.Context
			}
			if name == "" {
				name = "?"
			}
			st := c.Conclusion
			if st == "" {
				st = c.State
			}
			if st == "" {
				st = c.Status
			}
			switch st {
			case "QUEUED", "IN_PROGRESS", "PENDING", "EXPECTED":
				st = "PENDING"
			}
			url := c.DetailsURL
			if url == "" {
				url = c.TargetURL
			}
			x := mergecheck.Context{Name: name, State: st}
			if m := buildRE.FindStringSubmatch(url); m != nil {
				x.Build = m[1]
			}
			f.Contexts = append(f.Contexts, x)
		}
	}
	p.Facts = f
	p.Arch = archOf(g)
	p.Automaton = automatonOf(g)
	return p
}

// archOf is the latest architect verdict in the PR's comments, and whether a
// hold (a pass in progress) is newer than it.
func archOf(g ghNode) *mergecheck.Arch {
	var a *mergecheck.Arch
	holdAt := ""
	for _, c := range g.Comments.Nodes {
		if c.Author == nil || c.Author.Login != archBot && c.Author.Login != strings.TrimSuffix(archBot, "[bot]") {
			continue
		}
		switch {
		case strings.HasPrefix(c.Body, "<!-- architect-review-hold -->"):
			if c.CreatedAt > holdAt {
				holdAt = c.CreatedAt
			}
		case strings.HasPrefix(c.Body, "<!-- architect-review -->"):
			h := archHeadRE.FindStringSubmatch(c.Body)
			if h == nil || (a != nil && c.CreatedAt <= a.At) {
				continue
			}
			v := "UNREAD"
			if m := archVerdictRE.FindStringSubmatch(c.Body); m != nil {
				v = m[1]
			}
			a = &mergecheck.Arch{Verdict: v, Sha: h[2], At: c.CreatedAt}
		}
	}
	if a == nil && holdAt == "" {
		return nil
	}
	if a == nil {
		a = &mergecheck.Arch{}
	}
	a.HoldAt = holdAt
	a.Hold = holdAt != "" && (a.At == "" || holdAt > a.At)
	return a
}

// automatonOf is the Automaton's latest verdict on the PR, from its marker.
func automatonOf(g ghNode) *Verdict {
	var v *Verdict
	for _, c := range g.Comments.Nodes {
		m := automatonRE.FindStringSubmatch(c.Body)
		if m == nil || m[1] != fmt.Sprint(g.Number) || (v != nil && c.CreatedAt <= v.At) {
			continue
		}
		v = &Verdict{Verdict: strings.ToUpper(m[3]), Head: short(m[2]), At: c.CreatedAt}
	}
	return v
}

// searchPage is one search answer: each alias's nodes and paging.
type searchPage map[string]struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []ghNode `json:"nodes"`
}

func decodeSearch(data json.RawMessage) (searchPage, error) {
	var r searchPage
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	return r, nil
}

// decodeBatch reads a batchQuery answer: PRs, and the numbers that are issues.
// A number the repo does not have, or that is a PR, is absent from issues.
func decodeBatch(data json.RawMessage) ([]PR, []Issue, error) {
	var r struct {
		Repository map[string]*ghNode `json:"repository"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, nil, fmt.Errorf("decode batch: %w", err)
	}
	var prs []PR
	var issues []Issue
	for alias, n := range r.Repository {
		if n == nil || n.Number == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(alias, "p"):
			prs = append(prs, toPR(*n))
		case n.Typename == "Issue":
			issues = append(issues, toIssue(*n))
		}
	}
	return prs, issues, nil
}

func decodeFiles(data json.RawMessage) ([]string, string, error) {
	var r struct {
		Repository struct {
			PullRequest *struct {
				Files filesPage `json:"files"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, "", fmt.Errorf("decode files: %w", err)
	}
	if r.Repository.PullRequest == nil {
		return nil, "", nil
	}
	var out []string
	fp := r.Repository.PullRequest.Files
	for _, n := range fp.Nodes {
		out = append(out, n.Path)
	}
	more := ""
	if fp.PageInfo.HasNextPage {
		more = fp.PageInfo.EndCursor
	}
	return out, more, nil
}
