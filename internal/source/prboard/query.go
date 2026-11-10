package prboard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/mergecheck"
)

// prFields is everything a pass reads about one PR, in one GraphQL selection.
// The architect verdict is in the PR's own comments: a new bot comment bumps
// the PR's updatedAt, so the incremental search sees it without polling.
const prFields = `number title url state isDraft mergeable mergeStateStatus headRefOid headRefName baseRefName
additions deletions createdAt updatedAt mergedAt closedAt mergeCommit{oid} reviewDecision
closingIssuesReferences(first:5){nodes{number}}
latestOpinionatedReviews(first:20){nodes{author{login} state commit{oid}}}
reviewThreads(first:100){nodes{isResolved}}
timelineItems(itemTypes:[READY_FOR_REVIEW_EVENT], last:1){nodes{... on ReadyForReviewEvent{createdAt}}}
comments(last:30){nodes{author{login} createdAt body}}
commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:60){nodes{
... on CheckRun{name conclusion status detailsUrl} ... on StatusContext{context state targetUrl}}}}}}}`

// searchQuery is ONE search page: the author's PRs in repo updated at or after
// the cursor (every state, so merges and closes are seen), or, with no cursor,
// every open one.
func searchQuery(repo, author, cursor, after string) string {
	q := fmt.Sprintf("repo:%s is:pr author:%s", repo, author)
	if cursor == "" {
		q += " is:open"
	} else {
		q += " updated:>=" + cursor
	}
	pg := ""
	if after != "" {
		pg = fmt.Sprintf(`, after:%q`, after)
	}
	return fmt.Sprintf(`{search(type:ISSUE, first:%d%s, query:%q){pageInfo{hasNextPage endCursor} nodes{... on PullRequest{%s}}}}`,
		Batch, pg, q, prFields)
}

// batchQuery reads named PRs in one query (aliases p<n>), at most Batch.
func batchQuery(repo string, numbers []int) string {
	owner, name, _ := strings.Cut(repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "{repository(owner:%q, name:%q){", owner, name)
	for _, n := range numbers {
		fmt.Fprintf(&b, "p%d: pullRequest(number:%d){%s} ", n, n, prFields)
	}
	b.WriteString("}}")
	return b.String()
}

// ghPR is the GraphQL shape of prFields.
type ghPR struct {
	Number           int    `json:"number"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	HeadRefOid       string `json:"headRefOid"`
	HeadRefName      string `json:"headRefName"`
	BaseRefName      string `json:"baseRefName"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
	MergedAt         string `json:"mergedAt"`
	ClosedAt         string `json:"closedAt"`
	MergeCommit      *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	ReviewDecision string `json:"reviewDecision"`
	Closing        struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Reviews struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
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
	Timeline struct {
		Nodes []struct {
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	Comments struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt string `json:"createdAt"`
			Body      string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
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

// PR is one PR's facts as a pass read them.
type PR struct {
	Number    int              `json:"n"`
	Title     string           `json:"title"`
	URL       string           `json:"url"`
	Branch    string           `json:"branch"`
	Base      string           `json:"base"`
	Additions int              `json:"additions"`
	Deletions int              `json:"deletions"`
	Created   string           `json:"created"`
	Updated   string           `json:"updated"`
	ReadyAt   string           `json:"ready_at,omitempty"`
	MergedAt  string           `json:"merged_at,omitempty"`
	ClosedAt  string           `json:"closed_at,omitempty"`
	MergeSha  string           `json:"merge_sha,omitempty"`
	Review    string           `json:"review_decision,omitempty"`
	Checks    string           `json:"checks,omitempty"` // rollup state
	Closing   []int            `json:"closing,omitempty"`
	Facts     mergecheck.Facts `json:"facts"`
	Arch      *mergecheck.Arch `json:"architect,omitempty"`
}

var buildRE = regexp.MustCompile(`buildId=(\d+)`)

// The architect-review[bot] verdict comment and its hold marker (minister-control graph.py, same patterns).
var (
	archHeadRE    = regexp.MustCompile("Architect review\\s*[\u2014-]+\\s*PR #(\\d+)\\s*@\\s*`([0-9a-f]{7,40})`")
	archVerdictRE = regexp.MustCompile(`(?m)^\*\*([A-Z_]+)\*\*\s*$`)
)

const archBot = "architect-review[bot]"

func toPR(g ghPR) PR {
	p := PR{Number: g.Number, Title: g.Title, URL: g.URL, Branch: g.HeadRefName, Base: g.BaseRefName, Additions: g.Additions,
		Deletions: g.Deletions, Created: g.CreatedAt, Updated: g.UpdatedAt, MergedAt: g.MergedAt, ClosedAt: g.ClosedAt,
		Review: g.ReviewDecision, ReadyAt: g.CreatedAt}
	if g.MergeCommit != nil {
		p.MergeSha = g.MergeCommit.Oid
	}
	for _, n := range g.Timeline.Nodes {
		if n.CreatedAt != "" {
			p.ReadyAt = n.CreatedAt
		}
	}
	for _, n := range g.Closing.Nodes {
		p.Closing = append(p.Closing, n.Number)
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
		oid := ""
		if r.Commit != nil {
			oid = r.Commit.Oid
		}
		f.Reviews = append(f.Reviews, mergecheck.Review{Login: r.Author.Login, State: r.State, Oid: oid})
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
	return p
}

// archOf is the latest architect verdict in the PR's comments, and whether a
// hold (a pass in progress) is newer than it.
func archOf(g ghPR) *mergecheck.Arch {
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

// decodeSearch reads one search page.
func decodeSearch(data json.RawMessage) ([]PR, bool, string, error) {
	var r struct {
		Search struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []ghPR `json:"nodes"`
		} `json:"search"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, false, "", fmt.Errorf("decode search: %w", err)
	}
	var out []PR
	for _, n := range r.Search.Nodes {
		if n.Number != 0 {
			out = append(out, toPR(n))
		}
	}
	return out, r.Search.PageInfo.HasNextPage, r.Search.PageInfo.EndCursor, nil
}

// decodeBatch reads a batchQuery answer; a PR the repo no longer has is absent.
func decodeBatch(data json.RawMessage) ([]PR, error) {
	var r struct {
		Repository map[string]*ghPR `json:"repository"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode batch: %w", err)
	}
	var out []PR
	for _, n := range r.Repository {
		if n != nil && n.Number != 0 {
			out = append(out, toPR(*n))
		}
	}
	return out, nil
}
