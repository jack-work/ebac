package scout

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/mergecheck"
	"github.com/gluck/ebac/internal/model"
)

// prFields is everything a pass reads about one PR, in one GraphQL selection:
// the entry's facts, the merge checklist's facts, the relevance filter's
// labels and first page of files, and the body's issue links. The architect
// and automaton verdicts are PR comments: a new one bumps the PR's updatedAt,
// so the incremental search sees it without polling.
const prFields = `number title url state isDraft mergeable mergeStateStatus headRefOid baseRefName
createdAt updatedAt mergedAt closedAt additions deletions reviewDecision author{login} body labels(first:20){nodes{name}}
timelineItems(itemTypes:[READY_FOR_REVIEW_EVENT], last:1){nodes{__typename ... on ReadyForReviewEvent{createdAt}}}
files(first:100){totalCount pageInfo{hasNextPage endCursor} nodes{path}}
closingIssuesReferences(first:10){nodes{number}}
latestOpinionatedReviews(first:20){nodes{author{login} state submittedAt commit{oid}}}
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

// subFields are an issue's place in the issue tree (parent, sub-issues). A
// forge without sub-issues refuses them (noSubIssues): the scout then asks
// without them from that pass on.
const subFields = ` parent{number} subIssues(first:50){nodes{number}}`

func issueSel(subs bool) string {
	if subs {
		return issueFields + subFields
	}
	return issueFields
}

// noSubIssues says whether a GraphQL error is the forge not knowing the
// sub-issue fields.
func noSubIssues(err error) bool {
	s := err.Error()
	return strings.Contains(s, "subIssues") || strings.Contains(s, "'parent'") || strings.Contains(s, "\"parent\"")
}

// claimFields are what a claim needs of an issue: the claimer's comments
// (with bodies, to find the claim phrase), labels, state, and the PRs that
// close it with their state and files.
const claimFields = `number title state closedAt updatedAt labels(first:20){nodes{name}}
comments(last:50){nodes{author{login} createdAt body}}
closedByPullRequestsReferences(first:5){nodes{number state mergedAt closedAt files(first:50){nodes{path}}}}`

// alias is one aliased search of the scout's one search call.
type alias struct {
	name, query string
	claims      bool // claim selection (claimFields) instead of the author one
}

// searches are the pass's aliased searches: one per author (a<i>: PRs and
// issues), and with claims configured c0 (the claimer's phrase in comments)
// and c1 (the claim label). With no cursor, open items only.
func searches(w model.ScoutWatch, cursor string) []alias {
	since := " is:open"
	if cursor != "" {
		since = " updated:>=" + cursor
	}
	var out []alias
	for i, a := range w.Authors {
		out = append(out, alias{name: fmt.Sprintf("a%d", i), query: fmt.Sprintf("repo:%s author:%s", w.Repo, a) + since})
	}
	if w.ClaimBy != "" {
		phrase := w.ClaimPhrase
		if phrase == "" {
			phrase = "Praxis claimed"
		}
		out = append(out, alias{name: "c0", claims: true,
			query: fmt.Sprintf("repo:%s is:issue commenter:%s %q in:comments", w.Repo, w.ClaimBy, phrase) + since})
	}
	if w.ClaimLabel != "" {
		out = append(out, alias{name: "c1", claims: true, query: fmt.Sprintf("repo:%s is:issue label:%s", w.Repo, w.ClaimLabel) + since})
	}
	return out
}

// searchQuery is ONE GraphQL call: every alias, each its own search. after
// holds the page cursor of an alias still paging; a nil after map is the
// first page of every alias, and an alias absent from a non-nil map is done.
func searchQuery(as []alias, after map[string]string, subs bool, first int) string {
	var b strings.Builder
	b.WriteString("{")
	for _, a := range as {
		pg := ""
		if after != nil {
			c, ok := after[a.name]
			if !ok {
				continue
			}
			pg = fmt.Sprintf(`, after:%q`, c)
		}
		sel := fmt.Sprintf(`... on PullRequest{%s} ... on Issue{%s}`, prFields, issueSel(subs))
		if a.claims {
			sel = fmt.Sprintf(`... on Issue{%s}`, claimFields)
		}
		fmt.Fprintf(&b, `%s: search(type:ISSUE, first:%d%s, query:%q){pageInfo{hasNextPage endCursor} nodes{__typename %s}} `,
			a.name, first, pg, a.query, sel)
	}
	b.WriteString("}")
	return b.String()
}

// batchQuery reads named PRs (aliases p<n>) and issue-or-PR numbers (aliases
// i<n>) in one query (callers bound the count: SearchPage PRs, Batch issues).
func batchQuery(repo string, prs, issues []int, subs bool) string {
	owner, name, _ := strings.Cut(repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "{repository(owner:%q, name:%q){", owner, name)
	for _, n := range prs {
		fmt.Fprintf(&b, "p%d: pullRequest(number:%d){%s} ", n, n, prFields)
	}
	for _, n := range issues {
		fmt.Fprintf(&b, "i%d: issueOrPullRequest(number:%d){__typename ... on Issue{%s}} ", n, n, issueSel(subs))
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
	Typename         string `json:"__typename"`
	Number           int    `json:"number"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	ReviewDecision   string `json:"reviewDecision"`
	HeadRefOid       string `json:"headRefOid"`
	BaseRefName      string `json:"baseRefName"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Parent           *struct {
		Number int `json:"number"`
	} `json:"parent"`
	SubIssues struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"subIssues"`
	MergedAt string    `json:"mergedAt"`
	ClosedAt string    `json:"closedAt"`
	Author   *login    `json:"author"`
	Body     string    `json:"body"`
	Labels   labels    `json:"labels"`
	Files    filesPage `json:"files"`
	Closing  struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Reviews struct {
		Nodes []struct {
			Author      *login `json:"author"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
			Commit      *struct {
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
	ClosedBy struct {
		Nodes []struct {
			Number   int       `json:"number"`
			State    string    `json:"state"`
			MergedAt string    `json:"mergedAt"`
			ClosedAt string    `json:"closedAt"`
			Files    filesPage `json:"files"`
		} `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
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
	Created   string
	Updated   string
	ReadyAt   string // the last ready-for-review, else created when it was never a draft
	Additions int
	Deletions int
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
	Parent   int
	Subs     []int
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
	archHoldRE    = regexp.MustCompile("Head:\\s*`?([0-9a-f]{7,40})")
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
	if g.Parent != nil {
		is.Parent = g.Parent.Number
	}
	for _, c := range g.SubIssues.Nodes {
		is.Subs = append(is.Subs, c.Number)
	}
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
	p := PR{Number: g.Number, Author: g.author(), Title: g.Title, Base: g.BaseRefName, Created: g.CreatedAt, Updated: g.UpdatedAt,
		MergedAt: g.MergedAt, ClosedAt: g.ClosedAt, Labels: g.Labels.names(), Links: links(g), Additions: g.Additions, Deletions: g.Deletions}
	for _, f := range g.Files.Nodes {
		p.Files = append(p.Files, f.Path)
	}
	if g.Files.PageInfo.HasNextPage {
		p.FilesMore = g.Files.PageInfo.EndCursor
	}
	if !g.IsDraft {
		p.ReadyAt = g.CreatedAt
	}
	for _, e := range g.Timeline.Nodes {
		if e.Typename == "ReadyForReviewEvent" && e.CreatedAt != "" {
			p.ReadyAt = e.CreatedAt
		}
	}
	f := mergecheck.Facts{Oid: g.HeadRefOid, State: g.State, Draft: g.IsDraft, Heavy: true, Mergeable: g.Mergeable,
		MergeState: g.MergeStateStatus, Review: g.ReviewDecision, Contexts: []mergecheck.Context{}, Reviews: []mergecheck.Review{}}
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
		f.Reviews = append(f.Reviews, mergecheck.Review{Login: who, State: r.State, Oid: oid, At: r.SubmittedAt})
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
	holdAt, holdSha := "", ""
	for _, c := range g.Comments.Nodes {
		if c.Author == nil || c.Author.Login != archBot && c.Author.Login != strings.TrimSuffix(archBot, "[bot]") {
			continue
		}
		switch {
		case strings.HasPrefix(c.Body, "<!-- architect-review-hold -->"):
			if c.CreatedAt > holdAt {
				holdAt, holdSha = c.CreatedAt, ""
				if m := archHoldRE.FindStringSubmatch(c.Body); m != nil {
					holdSha = m[1]
				}
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
	a.HoldAt, a.HoldSha = holdAt, holdSha
	a.Hold = holdAt != "" && (a.At == "" || holdAt > a.At)
	return a
}

// automatonOf is the Automaton's latest verdict on the PR, from its marker.
func automatonOf(g ghNode) *Verdict {
	var v *Verdict
	for _, c := range g.Comments.Nodes {
		m := automatonRE.FindStringSubmatch(c.Body)
		if m == nil || m[1] != fmt.Sprint(g.Number) || (v != nil && unix(c.CreatedAt) <= v.At) {
			continue
		}
		v = &Verdict{Verdict: strings.ToUpper(m[3]), Head: short(m[2]), At: unix(c.CreatedAt)}
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

// KindFiles are the changed files the band-jobs step classifies: all of them
// when one page held them, else the first page only when it already shows
// code (the rest cannot make it docs or tests), else nil (not known).
func (p PR) KindFiles() []string {
	if p.FilesMore == "" {
		if p.Files == nil {
			return []string{}
		}
		return p.Files
	}
	if mergecheck.KindOf(p.Files) == "code" {
		return p.Files
	}
	return nil
}

// Claim is one Praxis claim as a pass read it: the issue, the claimer's
// newest claim comment, the PR that closes it (if any).
type Claim struct {
	Issue    int
	Title    string
	State    string // the issue's: OPEN, CLOSED
	ClosedAt string
	At       string // the claim comment's time ("" when only the label says so)
	Label    bool
	PR       int
	PRState  string // OPEN, MERGED, CLOSED
	PRDone   string // the PR's merge or close time
	Files    []string
}

// toClaim reads a claim-search node; ok false when it is not a claim (no
// claim comment by, no claim label).
func toClaim(n ghNode, by, phrase, label string) (Claim, bool) {
	if phrase == "" {
		phrase = "Praxis claimed"
	}
	c := Claim{Issue: n.Number, Title: n.Title, State: n.State, ClosedAt: n.ClosedAt}
	for _, cm := range n.Comments.Nodes {
		if by != "" && cm.Author != nil && strings.EqualFold(cm.Author.Login, by) && strings.Contains(cm.Body, phrase) && cm.CreatedAt > c.At {
			c.At = cm.CreatedAt
		}
	}
	for _, l := range n.Labels.names() {
		if label != "" && strings.EqualFold(l, label) {
			c.Label = true
		}
	}
	// The PR Praxis opened for it: an open one first, else a merged one, else any; the newest of a kind.
	rank := map[string]int{"OPEN": 3, "MERGED": 2, "CLOSED": 1}
	best := -1
	for i, p := range n.ClosedBy.Nodes {
		if best < 0 || rank[p.State] > rank[n.ClosedBy.Nodes[best].State] ||
			rank[p.State] == rank[n.ClosedBy.Nodes[best].State] && p.Number > n.ClosedBy.Nodes[best].Number {
			best = i
		}
	}
	if best >= 0 {
		p := n.ClosedBy.Nodes[best]
		c.PR, c.PRState = p.Number, p.State
		c.PRDone = p.MergedAt
		if c.PRDone == "" {
			c.PRDone = p.ClosedAt
		}
		for _, f := range p.Files.Nodes {
			c.Files = append(c.Files, f.Path)
		}
	}
	return c, c.At != "" || c.Label
}

// Status is the claim's state: merged (its PR merged), closed (the issue or
// its PR closed without a merge), pr-open (a PR is open, or the label says
// so), else claimed.
func (c Claim) Status() string {
	switch {
	case c.PRState == "MERGED":
		return "merged"
	case c.State == "CLOSED" || c.PRState == "CLOSED":
		return "closed"
	case c.PRState == "OPEN" || c.Label:
		return "pr-open"
	}
	return "claimed"
}

// Grouped are paths by directory ({"dir/": ["name", ...]}, a top-level file
// under ""), the first max paths, in their order within a directory.
func Grouped(paths []string, max int) map[string][]string {
	if len(paths) == 0 {
		return nil
	}
	out := map[string][]string{}
	for i, p := range paths {
		if i == max {
			break
		}
		dir, name := "", p
		if j := strings.LastIndex(p, "/"); j >= 0 {
			dir, name = p[:j+1], p[j+1:]
		}
		out[dir] = append(out[dir], name)
	}
	return out
}
