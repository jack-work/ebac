// Package ghcache is the GitHub cache, slice 1 (the PR read side), source
// github-cache in ebac-gh. The register is the critic's own role form: the
// holder edits config ({repos, rules, ttl}); the plugin derives track (one
// object: the index of tracked PRs). Each tracked PR is one child form
// ebac-<critic>-<repo>-pr-<n> holding gh (GitHub's state), parsed (what the
// parsers derive from it) and sync (version, cost, errors). A round reads
// pull heads over ls-refs (0 quota), then one lean aliased marks query, then
// a deep aliased read of only the PRs whose marks moved.
package ghcache

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Tier cadences: how often a tracked PR's marks are read (DESIGN 4).
const (
	HotEvery  = 5 * time.Minute
	WarmEvery = 30 * time.Minute
	ColdEvery = 6 * time.Hour
)

// DefaultClosedTTL is how long a closed, merged or no longer tracked PR keeps
// its form before it is retired (config.ttl.closed). One day, not DESIGN 1a's
// 7: one author closes ~200 PRs a week (bic/aether, 2026-11-20), and every
// kept form counts against the core's MaxChildren.
const DefaultClosedTTL = 24 * time.Hour

// DefaultMax and MaxMax bound the forms the cache keeps (config.max): well
// under the core's MaxChildren (100), so a burst of new PRs plus this round's
// retires never takes a reply past it (the core refuses the whole reply).
const (
	DefaultMax = 80
	MaxMax     = 90
)

// MaxRules bounds config.rules: each rule is at least one search alias.
const MaxRules = 10

// Config is the register's holder-edited config key.
type Config struct {
	Repos []string `json:"repos"`
	Rules []Rule   `json:"rules"`
	Max   int      `json:"max,omitempty"` // forms kept at most; 0 is DefaultMax
	TTL   struct {
		Closed string `json:"closed,omitempty"` // a Go duration; "" is DefaultClosedTTL
	} `json:"ttl,omitzero"`
	// Maintainer, Scope, WriteFields and ApprovePRs are accepted and never
	// used: the cache writes and wakes only by ebac's trusted config,
	// and the core flags these in ebac ls (form config ignored).
	Maintainer  json.RawMessage `json:"maintainer,omitempty"`
	Scope       json.RawMessage `json:"scope,omitempty"`
	WriteFields json.RawMessage `json:"write_fields,omitempty"`
	ApprovePRs  json.RawMessage `json:"approve_prs,omitempty"`
}

// Rule tracks a PR when any of its selectors matches: an author, a label, a
// title tag, or an explicit number. Repo, when set, scopes the rule to one of
// config.repos; explicit numbers need it (a number names a PR in one repo).
// Tier is hot (default), warm or cold.
type Rule struct {
	Repo     string   `json:"repo,omitempty"`
	Author   []string `json:"author,omitempty"`
	Label    []string `json:"label,omitempty"`
	TitleTag []string `json:"title_tag,omitempty"`
	PRs      []int    `json:"prs,omitempty"`
	Tier     string   `json:"tier,omitempty"`
}

var (
	// childKeyRe is the core's child key rule (internal/poll childKey).
	childKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	loginRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	// A label or title tag goes inside a quoted search term: no quote, no
	// backslash, no newline, at most 100 bytes.
	termRe = regexp.MustCompile(`^[^"\\\n\r]{1,100}$`)
)

// Every names a tier's cadence; ok is false for an unknown tier.
func Every(tier string) (time.Duration, bool) {
	switch tier {
	case "hot", "":
		return HotEvery, true
	case "warm":
		return WarmEvery, true
	case "cold":
		return ColdEvery, true
	}
	return 0, false
}

// ParseConfig reads and checks the config key. Absent is an error: a cache
// with no repos tracks nothing and would retire every form.
func ParseConfig(raw json.RawMessage) (*Config, time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, fmt.Errorf("config: absent (want {repos, rules})")
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, 0, fmt.Errorf("config: %w", err)
	}
	if len(c.Repos) == 0 {
		return nil, 0, fmt.Errorf("config: no repos")
	}
	keys := map[string]string{}
	for _, r := range c.Repos {
		if !repoRe.MatchString(r) {
			return nil, 0, fmt.Errorf("config: repo %q is not owner/name", r)
		}
		k := repoKey(r)
		if ck := ChildKey(r, 9999999999); !childKeyRe.MatchString(ck) {
			return nil, 0, fmt.Errorf("config: repo %s gives child key %q, not [a-z0-9][a-z0-9-], 1-100", r, ck)
		}
		if other, ok := keys[k]; ok {
			return nil, 0, fmt.Errorf("config: repos %s and %s share the form name part %q", other, r, k)
		}
		keys[k] = r
	}
	if len(c.Rules) == 0 || len(c.Rules) > MaxRules {
		return nil, 0, fmt.Errorf("config: %d rules (want 1-%d)", len(c.Rules), MaxRules)
	}
	for i, r := range c.Rules {
		if len(r.Author)+len(r.Label)+len(r.TitleTag)+len(r.PRs) == 0 {
			return nil, 0, fmt.Errorf("config: rule %d selects nothing", i)
		}
		if r.Repo != "" && keys[repoKey(r.Repo)] != r.Repo {
			return nil, 0, fmt.Errorf("config: rule %d: repo %q is not in repos", i, r.Repo)
		}
		if len(r.PRs) > 0 && r.Repo == "" {
			return nil, 0, fmt.Errorf("config: rule %d: prs need a repo", i)
		}
		if _, ok := Every(r.Tier); !ok {
			return nil, 0, fmt.Errorf("config: rule %d: tier %q (want hot, warm or cold)", i, r.Tier)
		}
		for _, a := range r.Author {
			if !loginRe.MatchString(a) {
				return nil, 0, fmt.Errorf("config: rule %d: author %q is not a login", i, a)
			}
		}
		for _, t := range append(append([]string{}, r.Label...), r.TitleTag...) {
			if !termRe.MatchString(t) {
				return nil, 0, fmt.Errorf("config: rule %d: term %q (1-100 bytes, no quote, backslash or newline)", i, t)
			}
		}
		for _, n := range r.PRs {
			if n <= 0 {
				return nil, 0, fmt.Errorf("config: rule %d: pr %d", i, n)
			}
		}
	}
	if c.Max == 0 {
		c.Max = DefaultMax
	}
	if c.Max < 1 || c.Max > MaxMax {
		return nil, 0, fmt.Errorf("config: max %d (want 1-%d)", c.Max, MaxMax)
	}
	ttl := DefaultClosedTTL
	if c.TTL.Closed != "" {
		d, err := time.ParseDuration(c.TTL.Closed)
		if err != nil || d <= 0 {
			return nil, 0, fmt.Errorf("config: ttl.closed %q is not a positive duration", c.TTL.Closed)
		}
		ttl = d
	}
	return &c, ttl, nil
}

// repoKey is a repo's part of a child key: its name, lower case, every byte
// outside [a-z0-9-] a dash (bic/aether -> aether).
func repoKey(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	b := []byte(strings.ToLower(name))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b)
}

// ChildKey is a tracked PR's child form key: <repo>-pr-<n> (the core names
// the form ebac-<critic>-<key>).
func ChildKey(repo string, n int) string { return fmt.Sprintf("%s-pr-%d", repoKey(repo), n) }
