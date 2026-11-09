package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
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
}

func (a AuthoredPR) Key() string { return fmt.Sprintf("%s#%d", a.Repo, a.Number) }

// ListAuthoredPRs lists one author's most recently updated PRs in a repo,
// at most limit, updated since a time, every state. A busy author's older PRs
// are simply not fetched: the window is the newest limit, never a failure
// (author-chiqun passed 200 PRs on 2026-11-09).
func (g *GH) ListAuthoredPRs(ctx context.Context, repo, login string, since time.Time, limit int) ([]AuthoredPR, error) {
	args := []string{"pr", "list", "--repo", repo, "--author", login, "--state", "all",
		"--search", "updated:>=" + since.UTC().Format(time.RFC3339) + " sort:updated-desc", "--limit", fmt.Sprint(limit),
		"--json", "number,url,title,state,isDraft,headRefOid,createdAt,updatedAt,mergedAt"}
	out, err := g.run(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		State     string `json:"state"`
		IsDraft   bool   `json:"isDraft"`
		Head      string `json:"headRefOid"`
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
		MergedAt  string `json:"mergedAt"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode pr list: %w", err)
	}
	prs := make([]AuthoredPR, 0, len(rows))
	for _, r := range rows {
		title, _ := model.CutBody(r.Title, authorTitleBytes)
		head := r.Head
		if len(head) > 12 {
			head = head[:12]
		}
		merged := r.MergedAt
		if strings.HasPrefix(merged, "0001-") {
			merged = ""
		}
		prs = append(prs, AuthoredPR{Repo: repo, Number: r.Number, URL: r.URL, Title: title, State: r.State,
			Draft: r.IsDraft, Head: head, Created: r.CreatedAt, Updated: r.UpdatedAt, Merged: merged})
	}
	return prs, nil
}
