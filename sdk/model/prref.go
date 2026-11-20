package model

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type PRRef struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Author  string `json:"author,omitempty"`
	IsDraft bool   `json:"is_draft,omitempty"`
}

func (r PRRef) Key() PRKey { return MakePRKey(r.Owner, r.Repo, r.Number) }

// HostOf is the forge named by the ref's URL, or fallback when it carries none.
func (r PRRef) HostOf(fallback string) string {
	if u, err := url.Parse(r.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return fallback
}

func (r PRRef) Slug() string { return fmt.Sprintf("%s/%s", r.Owner, r.Repo) }

// forgeHost is the single forge a set of refs lives on. A critic polls one host.
func ForgeHost(refs []PRRef, fallback string) (string, error) {
	host := ""
	for _, r := range refs {
		h := r.HostOf(fallback)
		if host != "" && h != host {
			return "", fmt.Errorf("PRs span two forges (%s, %s): use one critic per forge", host, h)
		}
		host = h
	}
	if host == "" {
		return fallback, nil
	}
	return host, nil
}

// ParsePRRef accepts a browser URL or owner/repo#number.
func ParsePRRef(s string) (PRRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return PRRef{}, fmt.Errorf("empty PR reference")
	}
	if strings.Contains(s, "://") {
		u := strings.TrimSuffix(s, "/")
		parts := strings.Split(u, "/")
		// .../<owner>/<repo>/pull/<n>
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "pull" || parts[i] == "pulls" {
				if i < 2 {
					break
				}
				n, err := strconv.Atoi(parts[i+1])
				if err != nil {
					return PRRef{}, fmt.Errorf("bad PR number in %q", s)
				}
				return PRRef{Owner: parts[i-2], Repo: parts[i-1], Number: n, URL: s}, nil
			}
		}
		return PRRef{}, fmt.Errorf("not a pull request URL: %q", s)
	}
	slug, num, ok := strings.Cut(s, "#")
	if !ok {
		return PRRef{}, fmt.Errorf("expected owner/repo#number or a URL, got %q", s)
	}
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok {
		return PRRef{}, fmt.Errorf("expected owner/repo#number, got %q", s)
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return PRRef{}, fmt.Errorf("bad PR number in %q", s)
	}
	return PRRef{Owner: owner, Repo: repo, Number: n}, nil
}
