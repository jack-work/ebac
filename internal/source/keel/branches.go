package keel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// The branch reads keel-branches needs (keel viewer api.go: refs, span, log).
// Through ebac's door they need keel-lanes' allowlist (K1); over HTTPS they
// need a read token.

// Ref is one ref as keel lists it (GET /api/r/<id>/refs).
type Ref struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // branch | tag | remote
	SHA  string `json:"sha"`
	Time int64  `json:"time,omitempty"`
}

// Commit is one log entry (GET /api/r/<id>/log, and a span's base and head).
type Commit struct {
	SHA     string   `json:"sha"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Time    int64    `json:"time"`
	Subject string   `json:"subject"`
}

// Change is one file a span changes; Additions and Deletions are nil for a
// binary file.
type Change struct {
	Path        string `json:"path"`
	Status      string `json:"status"` // A M D R C T
	Additions   *int   `json:"additions"`
	Deletions   *int   `json:"deletions"`
	Binary      bool   `json:"binary"`
	RenamedFrom string `json:"renamedFrom,omitempty"`
}

// Span is GET /api/r/<id>/span?span=<base>...<head>: Base is the merge base,
// Count the commits from it to the head.
type Span struct {
	Base  *Commit  `json:"base"`
	Head  *Commit  `json:"head"`
	Count *int     `json:"count"`
	Files []Change `json:"files"`
}

func repoPath(repo, what string) string {
	return "/api/r/" + url.PathEscape(ViewerID(repo)) + "/" + what
}

// Refs lists the repo's refs. A body without refs is an error, never "none".
func (c *Client) Refs(ctx context.Context, repo string) ([]Ref, error) {
	body, err := c.get(ctx, repoPath(repo, "refs"), 0)
	if err != nil {
		return nil, err
	}
	var out struct {
		Refs *[]Ref `json:"refs"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Refs == nil {
		return nil, fmt.Errorf("keel GET refs: no refs member (%v): %s", err, snippet(body))
	}
	return *out.Refs, nil
}

// Span reads base...head: the merge base, the head, the commit count and the
// changed files.
func (c *Client) Span(ctx context.Context, repo, base, head string) (*Span, error) {
	body, err := c.get(ctx, repoPath(repo, "span")+"?"+url.Values{"span": {base + "..." + head}}.Encode(), 0)
	if err != nil {
		return nil, err
	}
	var sp Span
	if err := json.Unmarshal(body, &sp); err != nil || sp.Base == nil || sp.Head == nil || sp.Count == nil {
		return nil, fmt.Errorf("keel GET span %s...%s: want base, head and count (%v): %s", base, head, err, snippet(body))
	}
	return &sp, nil
}

// Log is the newest n commits reachable from rev (n at most 500).
func (c *Client) Log(ctx context.Context, repo, rev string, n int) ([]Commit, error) {
	body, err := c.get(ctx, repoPath(repo, "log")+"?"+url.Values{"rev": {rev}, "n": {strconv.Itoa(n)}}.Encode(), 0)
	if err != nil {
		return nil, err
	}
	var cs []Commit
	if err := json.Unmarshal(body, &cs); err != nil || cs == nil {
		return nil, fmt.Errorf("keel GET log %s: want a list (%v): %s", rev, err, snippet(body))
	}
	return cs, nil
}
