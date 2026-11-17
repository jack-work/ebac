package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// GitHub access goes through the authenticated `gh` CLI rather than a token
// this process holds. Two reasons, both load-bearing:
//   - GHE auth is already solved by `gh auth login --hostname`, including
//     SSO and token refresh, and nothing here has to store a secret.
//   - prangl did the same, for the same reason, and the alternative
//     (webhooks) needs repo admin and a reachable receiver.
type GH struct {
	Bin  string
	Host string
	// Timeout bounds a single API invocation.
	Timeout time.Duration
	// WriteGap spaces content-creating calls, as GitHub asks: serially, a
	// second apart. A burst of thread replies on 2026-10-20 left one of
	// them opening a review that never submitted.
	WriteGap  time.Duration
	lastWrite time.Time
	// Every call runs with a token minted by band-cred for the CURRENT Host
	// (the poller retargets Host per critic, so the credential is chosen per
	// call, not at construction). NoCred is the test seam for a fake gh.
	NoCred bool
	credMu sync.Mutex // concurrent reads (author PRFiles) share creds
	creds  map[string]*Cred
	// spend counts this process's GraphQL reads and their cost (gql.go).
	spendMu sync.Mutex
	spend   Spend
}

// cred is the band-cred credential for g.Host, one per host per process.
func (g *GH) cred() *Cred {
	g.credMu.Lock()
	defer g.credMu.Unlock()
	if g.creds == nil {
		g.creds = map[string]*Cred{}
	}
	c, ok := g.creds[g.Host]
	if !ok {
		c = credForHost(g.Host)
		g.creds[g.Host] = c
	}
	return c
}

// pace waits until WriteGap has passed since the last write.
func (g *GH) pace() {
	if wait := time.Until(g.lastWrite.Add(g.WriteGap)); wait > 0 {
		time.Sleep(wait)
	}
	g.lastWrite = time.Now()
}

func NewGH(bin, host string) *GH {
	if bin == "" {
		bin = "gh"
	}
	return &GH{Bin: bin, Host: host, Timeout: 90 * time.Second, WriteGap: time.Second}
}

// ghCreds names the band-cred credential for each forge host ebac may call.
// A host without one is refused by band-cred: never another host's token.
var ghCreds = map[string]string{"microsoft.ghe.com": CredGitHub}

// defaultForgeHost is the host a PR reference without one (owner/repo#N)
// resolves to: the one forge ebac holds a band-cred credential for. It was
// github.com, which ebac cannot reach since every call takes a band-cred token:
// three critics added from shorthand on 2026-10-24 failed every round.
func DefaultForgeHost() string {
	if len(ghCreds) == 1 {
		for h := range ghCreds {
			return h
		}
	}
	return "microsoft.ghe.com"
}

// checkForgeHost refuses, at creation, a host ebac holds no credential for:
// such a critic could never poll.
func CheckForgeHost(host string) error {
	if _, ok := ghCreds[host]; !ok {
		hosts := make([]string, 0, len(ghCreds))
		for h := range ghCreds {
			hosts = append(hosts, h)
		}
		return fmt.Errorf("no band-cred credential for forge host %q (ebac can poll: %s): give the full PR URL or --host", host, strings.Join(hosts, ", "))
	}
	return nil
}

func credForHost(host string) *Cred {
	name, ok := ghCreds[host]
	if !ok {
		name = "no-credential-for-" + host
	}
	return NewCred(os.Getenv("EBAC_BAND_CRED"), name)
}

// execCommandContext is exec.CommandContext, indirected so tests can fake an
// external `gh` without a shell shebang (which does not resolve inside the
// Nix build sandbox's checkPhase -- there is no /usr/bin/env or /bin/bash to
// find, so a script-based fake fails there with a confusing ENOENT on the
// fake's own path). Production never overrides this.
var ExecCommandContext = exec.CommandContext

func (g *GH) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	env := append(os.Environ(), "GH_HOST="+g.Host, "GH_PAGER=", "NO_COLOR=1")
	if !g.NoCred {
		tok, err := g.cred().Token(ctx)
		if err != nil {
			return nil, err
		}
		// GH_ENTERPRISE_TOKEN is what gh reads for a non-github.com host.
		env = append(env, "GH_TOKEN="+tok, "GH_ENTERPRISE_TOKEN="+tok)
	}
	g.spendMu.Lock()
	g.spend.All++
	g.spendMu.Unlock()
	cmd := ExecCommandContext(ctx, g.Bin, args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("gh timed out after %s: %s", g.Timeout, msg)
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(shownArgs(args), " "), msg)
	}
	return out.Bytes(), nil
}

// shownArgs are gh's arguments as an error shows them: a GraphQL query (or any
// -f/-F value past 120 bytes) by its size, never its text.
func shownArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok && (k == "query" || len(v) > 120) {
			a = fmt.Sprintf("%s=<%d B>", k, len(v))
		}
		out[i] = a
	}
	return out
}

func (g *GH) Whoami(ctx context.Context) (string, error) {
	out, err := g.run(ctx, []string{"api", "user", "-q", ".login"}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

const (
	stampMarker = "No person reviewed this change"
	stampWindow = 60 * time.Second
)

// markStamped flags person-login reviews that a bot produced: either the body
// says so, or a bot submitted the same state on the same PR just before.
func MarkStamped(reviews map[string]*model.Review) {
	for _, r := range reviews {
		if r.IsBot {
			continue
		}
		if strings.Contains(r.Body, stampMarker) {
			r.Stamped = true
			continue
		}
		at, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			continue
		}
		for _, b := range reviews {
			bat, err := time.Parse(time.RFC3339, b.SubmittedAt)
			if b.IsBot && b.State == r.State && err == nil && !bat.After(at) && at.Sub(bat) <= stampWindow {
				r.Stamped = true
				break
			}
		}
	}
}

// ListPRs finds pull requests matching a filter, for `ebac discover`.
func (g *GH) ListPRs(ctx context.Context, repo, author, state string, limit int) ([]model.PRRef, error) {
	args := []string{"pr", "list", "--repo", repo, "--state", state,
		"--limit", strconv.Itoa(limit), "--json", "number,url,title,isDraft,author,updatedAt"}
	if author != "" {
		args = append(args, "--author", author)
	}
	out, err := g.run(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		IsDraft   bool   `json:"isDraft"`
		UpdatedAt string `json:"updatedAt"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode pr list: %w", err)
	}
	owner, name, _ := strings.Cut(repo, "/")
	refs := make([]model.PRRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, model.PRRef{
			Owner: owner, Repo: name, Number: r.Number, URL: r.URL,
			Title: r.Title, Author: r.Author.Login, IsDraft: r.IsDraft,
		})
	}
	return refs, nil
}

// CreateIssue files a bug. Develop-mode arias use this to report defects in
// the harness itself rather than silently working around them.
func (g *GH) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (string, error) {
	args := []string{"issue", "create", "--repo", repo, "--title", title, "--body-file", "-"}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	out, err := g.run(ctx, args, []byte(body))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isBot decides authorship class. GraphQL's __typename is authoritative for
// GitHub Apps; the [bot] suffix and a name list catch the rest. A bot's
// comment is still recorded — it is only demoted below the wake threshold.
func isBot(a gqlAuthor) bool {
	if a.Typename == "Bot" || a.Typename == "EnterpriseUserAccount" {
		return true
	}
	l := strings.ToLower(a.Login)
	if strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") {
		return true
	}
	switch l {
	case "coderabbitai", "copilot", "copilot-pull-request-reviewer", "github-actions",
		"dependabot", "renovate", "azure-pipelines", "sonarcloud", "codecov":
		return true
	}
	return false
}

// Descends says whether head is a STRICT descendant of base in repo: the
// compare API answers "ahead" with behind_by 0. Behind, diverged, identical,
// and a compare the forge does not know (404, 422: an unknown sha, no merge
// base, or no compare at all) are a definite false, never an error; only a
// failed call is an error.
func (g *GH) Descends(ctx context.Context, repo, base, head string) (bool, error) {
	out, err := g.run(ctx, []string{"api", "repos/" + repo + "/compare/" + base + "..." + head,
		"-q", `.status + " " + (.behind_by|tostring)`}, nil)
	if err != nil {
		if m := err.Error(); strings.Contains(m, "HTTP 404") || strings.Contains(m, "HTTP 422") || strings.Contains(m, "Not Found") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "ahead 0", nil
}

// BranchTips is the sha each branch points at ("" when the branch is gone),
// all in one aliased GraphQL query per 100 branches (1 point): it was one
// REST call a branch (band-state: 41 every 5 min, 11-17).
func (g *GH) BranchTips(ctx context.Context, repo string, branches []string) (map[string]string, error) {
	owner, name, _ := strings.Cut(repo, "/")
	out := map[string]string{}
	const per = 100
	for lo := 0; lo < len(branches); lo += per {
		part := branches[lo:min(lo+per, len(branches))]
		var b strings.Builder
		fmt.Fprintf(&b, "query{ repository(owner:%s,name:%s){", strconv.Quote(owner), strconv.Quote(name))
		for i, br := range part {
			fmt.Fprintf(&b, " b%d: ref(qualifiedName:%s){ target{ oid } }", i, strconv.Quote("refs/heads/"+br))
		}
		b.WriteString(" } }")
		data, err := g.GraphQL(ctx, b.String())
		if err != nil {
			return nil, err
		}
		var r struct {
			Repository map[string]*struct {
				Target struct {
					Oid string `json:"oid"`
				} `json:"target"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("decode refs: %w", err)
		}
		for i, br := range part {
			out[br] = ""
			if x := r.Repository[fmt.Sprintf("b%d", i)]; x != nil {
				out[br] = x.Target.Oid
			}
		}
	}
	return out, nil
}

// GraphQL runs one GraphQL read and returns its data object (rateLimit
// stripped). A response with errors is an error naming the first one.
func (g *GH) GraphQL(ctx context.Context, query string) (json.RawMessage, error) {
	data, errs, err := g.query(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("graphql: %s", errs[0].Message)
	}
	return data, nil
}
