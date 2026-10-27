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
	creds  map[string]*Cred
}

// cred is the band-cred credential for g.Host, one per host per process.
func (g *GH) cred() *Cred {
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
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return out.Bytes(), nil
}

func (g *GH) Whoami(ctx context.Context) (string, error) {
	out, err := g.run(ctx, []string{"api", "user", "-q", ".login"}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// graphqlPRQuery fetches everything one round needs about one PR.
// reviews and reviewThreads are BOTH paginated with their own independent
// cursor; the caller must honour each hasNextPage or mark the snapshot
// incomplete. reviews used to be `reviews(last:30)` with no cursor at all --
// on any PR whose review count exceeds 30 (every long-running automated
// review loop gets there fast: bic/aether#24485 alone crossed a hundred
// individual review submissions) the reviews beyond the newest 30 were
// silently dropped, with nothing in the response distinguishing "no more
// reviews" from "we didn't ask for the rest." A critic could report "no new
// reviews" while reviews genuinely existed just outside the window -- a zero
// that meant "the instrument didn't look" rendered identically to a zero
// that meant "there was nothing there."
const graphqlPRQuery = `
query($owner:String!,$name:String!,$number:Int!,$cursor:String,$reviewsCursor:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      number url title state isDraft merged reviewDecision updatedAt baseRefName
      author{login __typename}
      headRefOid
      pending: reviews(states:PENDING, first:5){
        nodes{ id databaseId createdAt author{login} comments{ totalCount } }
      }
      reviews(first:50, after:$reviewsCursor){
        pageInfo{ hasNextPage endCursor }
        nodes{ id state submittedAt body author{login __typename} }
      }
      comments(first:100){
        pageInfo{ hasNextPage }
        nodes{ id url body createdAt updatedAt author{login __typename} }
      }
      reviewThreads(first:50, after:$cursor){
        pageInfo{ hasNextPage endCursor }
        nodes{
          id isResolved isOutdated path line
          comments(first:100){
            pageInfo{ hasNextPage }
            nodes{ id url body state createdAt updatedAt author{login __typename} }
          }
        }
      }
    }
  }
}`

type gqlAuthor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

type gqlResp struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				Number         int       `json:"number"`
				URL            string    `json:"url"`
				Title          string    `json:"title"`
				State          string    `json:"state"`
				IsDraft        bool      `json:"isDraft"`
				Merged         bool      `json:"merged"`
				ReviewDecision string    `json:"reviewDecision"`
				UpdatedAt      string    `json:"updatedAt"`
				BaseRefName    string    `json:"baseRefName"`
				Author         gqlAuthor `json:"author"`
				HeadRefOid     string    `json:"headRefOid"`
				Pending        struct {
					Nodes []struct {
						ID         string    `json:"id"`
						DatabaseID int64     `json:"databaseId"`
						CreatedAt  string    `json:"createdAt"`
						Author     gqlAuthor `json:"author"`
						Comments   struct {
							TotalCount int `json:"totalCount"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"pending"`
				Reviews struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID          string    `json:"id"`
						State       string    `json:"state"`
						SubmittedAt string    `json:"submittedAt"`
						Body        string    `json:"body"`
						Author      gqlAuthor `json:"author"`
					} `json:"nodes"`
				} `json:"reviews"`
				Comments struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID        string    `json:"id"`
						URL       string    `json:"url"`
						Body      string    `json:"body"`
						CreatedAt string    `json:"createdAt"`
						UpdatedAt string    `json:"updatedAt"`
						Author    gqlAuthor `json:"author"`
					} `json:"nodes"`
				} `json:"comments"`
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Comments   struct {
							PageInfo struct {
								HasNextPage bool `json:"hasNextPage"`
							} `json:"pageInfo"`
							Nodes []struct {
								ID        string    `json:"id"`
								URL       string    `json:"url"`
								Body      string    `json:"body"`
								State     string    `json:"state"`
								CreatedAt string    `json:"createdAt"`
								UpdatedAt string    `json:"updatedAt"`
								Author    gqlAuthor `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"errors"`
}

// FetchPR returns one PR's state. The returned PRState carries Complete=false
// if any page was left unread, which forbids the differ from inferring
// deletions for it.
func (g *GH) FetchPR(ctx context.Context, owner, repo string, number int) (*model.PRState, error) {
	st := &model.PRState{
		Key: model.MakePRKey(owner, repo, number), Owner: owner, Repo: repo, Number: number,
		Threads: map[string]*model.Thread{}, Reviews: map[string]*model.Review{}, Comments: map[string]*model.Comment{}, Complete: true,
	}

	cursor := ""
	reviewsCursor := ""
	for page := 0; ; page++ {
		if page > 40 {
			st.Complete = false
			break
		}
		args := []string{
			"api", "graphql",
			"-f", "query=" + graphqlPRQuery,
			"-F", "owner=" + owner,
			"-F", "name=" + repo,
			"-F", "number=" + strconv.Itoa(number),
			"-F", "cursor=" + cursor,
			"-F", "reviewsCursor=" + reviewsCursor,
		}
		out, err := g.run(ctx, args, nil)
		if err != nil {
			return nil, err
		}
		var r gqlResp
		if err := json.Unmarshal(out, &r); err != nil {
			return nil, fmt.Errorf("decode graphql: %w", err)
		}
		if len(r.Errors) > 0 {
			return nil, fmt.Errorf("graphql: %s", r.Errors[0].Message)
		}
		pr := r.Data.Repository.PullRequest
		if pr == nil {
			return nil, fmt.Errorf("%s/%s#%d not found", owner, repo, number)
		}

		// Reviews are paginated independently of reviewThreads, so this
		// runs on every page: once the reviews cursor is exhausted, later
		// pages (still walking through reviewThreads) simply repeat its
		// last, empty page, which is harmless.
		// Pending is asked for on every page and is the same each time.
		st.Pending = st.Pending[:0]
		for _, pn := range pr.Pending.Nodes {
			st.Pending = append(st.Pending, model.PendingReview{ID: pn.ID, DatabaseID: pn.DatabaseID,
				Author: pn.Author.Login, CreatedAt: pn.CreatedAt, Comments: pn.Comments.TotalCount})
		}
		for _, rv := range pr.Reviews.Nodes {
			st.Reviews[rv.ID] = &model.Review{
				ID: rv.ID, Author: rv.Author.Login, IsBot: isBot(rv.Author),
				State: rv.State, SubmittedAt: rv.SubmittedAt, Body: rv.Body, Digest: model.BodyDigest(rv.Body),
			}
		}

		if page == 0 {
			st.URL, st.Title, st.IsDraft = pr.URL, pr.Title, pr.IsDraft
			st.HeadSHA, st.BaseRef, st.UpdatedAt = pr.HeadRefOid, pr.BaseRefName, pr.UpdatedAt
			st.ReviewDecision = pr.ReviewDecision
			st.Author = pr.Author.Login
			st.State = pr.State
			if pr.Merged {
				st.State = "MERGED"
			}
			if pr.Comments.PageInfo.HasNextPage {
				// More than 100 top-level comments. Same rule as an
				// over-full thread: mark the scan partial rather than
				// silently truncate, which would make an unread comment
				// look deleted next round.
				st.Complete = false
			}
			for _, cn := range pr.Comments.Nodes {
				c := &model.Comment{
					ID: cn.ID, URL: cn.URL, Body: cn.Body,
					CreatedAt: cn.CreatedAt, UpdatedAt: cn.UpdatedAt,
					Author: cn.Author.Login, IsBot: isBot(cn.Author),
				}
				c.ComputeDigest()
				st.Comments[c.ID] = c
			}
		}

		for _, tn := range pr.ReviewThreads.Nodes {
			t := &model.Thread{ID: tn.ID, Path: tn.Path, Line: tn.Line, IsResolved: tn.IsResolved, IsOutdated: tn.IsOutdated}
			if tn.Comments.PageInfo.HasNextPage {
				// More than 100 comments on one thread. Rather than
				// silently truncate — which would make every unread
				// comment look deleted — mark the scan partial.
				st.Complete = false
			}
			for _, cn := range tn.Comments.Nodes {
				c := &model.Comment{
					ID: cn.ID, URL: cn.URL, Body: cn.Body,
					CreatedAt: cn.CreatedAt, UpdatedAt: cn.UpdatedAt,
					Author: cn.Author.Login, IsBot: isBot(cn.Author),
				}
				if cn.State == "PENDING" {
					c.State = "PENDING"
				}
				c.ComputeDigest()
				t.Comments = append(t.Comments, c)
			}
			st.Threads[t.ID] = t
		}

		threadsMore := pr.ReviewThreads.PageInfo.HasNextPage
		reviewsMore := pr.Reviews.PageInfo.HasNextPage
		if !threadsMore && !reviewsMore {
			break
		}
		if threadsMore {
			cursor = pr.ReviewThreads.PageInfo.EndCursor
		}
		if reviewsMore {
			reviewsCursor = pr.Reviews.PageInfo.EndCursor
		}
	}
	MarkStamped(st.Reviews)
	return st, nil
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
