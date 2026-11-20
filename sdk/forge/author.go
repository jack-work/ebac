package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

const authorTitleBytes = 120

// AuthoredPR is one PR on the form: short fields only.
type AuthoredPR struct {
	Repo    string `json:"repo"`
	Number  int    `json:"n"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	State   string `json:"state"` // OPEN, CLOSED, MERGED
	Draft   bool   `json:"draft,omitempty"`
	Head    string `json:"head"` // 12 hex
	Created string `json:"created"`
	Updated string `json:"updated"`
	Merged  string `json:"merged,omitempty"`
	Closed  string `json:"closed,omitempty"` // closedAt of a CLOSED (not merged) PR
}

func (a AuthoredPR) Key() string { return fmt.Sprintf("%s#%d", a.Repo, a.Number) }

// AuthorListLimit bounds each of the two lists an author watch reads per repo.
const AuthorListLimit = 200

// ListAuthoredPRs lists one author's PRs in a repo by STATE, not by recency:
// every OPEN PR updated since a time, plus every PR closed or merged since
// closedSince. Membership then changes only on a real transition; a window of
// the N most recently updated PRs dropped a quiet open PR whenever N others
// moved and re-added it on its next update (author-chiqun: 299 entries removed
// for 15 live in a day, 2026-11-13). Both searches ride ONE aliased GraphQL
// query (one point; it was two `gh pr list` calls), deduplicated by PR, at
// most AuthorListLimit each.
func (g *GH) ListAuthoredPRs(ctx context.Context, repo, login string, since, closedSince time.Time) ([]AuthoredPR, error) {
	upd := "updated:>=" + since.UTC().Format(time.RFC3339)
	base := fmt.Sprintf("repo:%s is:pr author:%s ", repo, login)
	searches := []string{
		base + "is:open " + upd + " sort:updated-desc",
		base + "is:closed closed:>=" + closedSince.UTC().Format(time.RFC3339) + " " + upd + " sort:updated-desc",
	}
	after := make([]string, len(searches))
	got := make([][]AuthoredPR, len(searches))
	for page := 0; page < AuthorListLimit/100+1; page++ {
		var b strings.Builder
		b.WriteString("query{")
		asked := 0
		for i, q := range searches {
			if page > 0 && after[i] == "" || len(got[i]) >= AuthorListLimit {
				continue
			}
			cur := ""
			if after[i] != "" {
				cur = ", after:" + strconv.Quote(after[i])
			}
			fmt.Fprintf(&b, "\n  s%d: search(type:ISSUE, first:100%s, query:%s){ pageInfo{ hasNextPage endCursor }"+
				" nodes{ ... on PullRequest { number url title state isDraft headRefOid createdAt updatedAt mergedAt closedAt } } }",
				i, cur, strconv.Quote(q))
			asked++
		}
		if asked == 0 {
			break
		}
		b.WriteString("\n}")
		data, err := g.GraphQL(ctx, b.String())
		if err != nil {
			return nil, err
		}
		var res map[string]struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []authoredRow `json:"nodes"`
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("decode search: %w", err)
		}
		for i := range searches {
			s, ok := res[fmt.Sprintf("s%d", i)]
			if !ok {
				continue
			}
			for _, r := range s.Nodes {
				if r.Number != 0 && len(got[i]) < AuthorListLimit {
					got[i] = append(got[i], r.pr(repo))
				}
			}
			after[i] = ""
			if s.PageInfo.HasNextPage {
				after[i] = s.PageInfo.EndCursor
			}
		}
	}
	seen := map[string]bool{}
	var out []AuthoredPR
	for _, pr := range append(got[0], got[1]...) {
		if !seen[pr.Key()] {
			seen[pr.Key()] = true
			out = append(out, pr)
		}
	}
	return out, nil
}

type authoredRow struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	IsDraft   bool   `json:"isDraft"`
	Head      string `json:"headRefOid"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	MergedAt  string `json:"mergedAt"`
	ClosedAt  string `json:"closedAt"`
}

func (r authoredRow) pr(repo string) AuthoredPR {
	title, _ := model.CutBody(r.Title, authorTitleBytes)
	head := r.Head
	if len(head) > 12 {
		head = head[:12]
	}
	closed := ""
	if r.State == "CLOSED" {
		closed = r.ClosedAt
	}
	merged := ""
	if r.State == "MERGED" {
		merged = r.MergedAt
	}
	return AuthoredPR{Repo: repo, Number: r.Number, URL: r.URL, Title: title, State: r.State,
		Draft: r.IsDraft, Head: head, Created: r.CreatedAt, Updated: r.UpdatedAt, Merged: merged, Closed: closed}
}

// PRFiles lists every file a pull request changes (paginated, so a PR of any
// size is complete).
func (g *GH) PRFiles(ctx context.Context, repo string, number int) ([]string, error) {
	out, err := g.run(ctx, []string{"api", "--paginate", fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", repo, number),
		"--jq", ".[].filename"}, nil)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}
