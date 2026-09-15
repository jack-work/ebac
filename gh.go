package main

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
}

func NewGH(bin, host string) *GH {
	if bin == "" {
		bin = "gh"
	}
	return &GH{Bin: bin, Host: host, Timeout: 90 * time.Second}
}

func (g *GH) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Env = append(os.Environ(), "GH_HOST="+g.Host, "GH_PAGER=", "NO_COLOR=1")
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
// reviewThreads and their comments are both paginated; the caller must
// honour hasNextPage or mark the snapshot incomplete.
const graphqlPRQuery = `
query($owner:String!,$name:String!,$number:Int!,$cursor:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      number url title state isDraft merged reviewDecision updatedAt baseRefName
      author{login __typename}
      headRefOid
      reviews(last:30){ nodes{ id state submittedAt body author{login __typename} } }
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
            nodes{ id url body createdAt updatedAt author{login __typename} }
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
				Reviews        struct {
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
func (g *GH) FetchPR(ctx context.Context, owner, repo string, number int) (*PRState, error) {
	st := &PRState{
		Key: MakePRKey(owner, repo, number), Owner: owner, Repo: repo, Number: number,
		Threads: map[string]*Thread{}, Reviews: map[string]*Review{}, Comments: map[string]*Comment{}, Complete: true,
	}

	cursor := ""
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
		}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		} else {
			args = append(args, "-F", "cursor=")
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

		if page == 0 {
			st.URL, st.Title, st.IsDraft = pr.URL, pr.Title, pr.IsDraft
			st.HeadSHA, st.BaseRef, st.UpdatedAt = pr.HeadRefOid, pr.BaseRefName, pr.UpdatedAt
			st.ReviewDecision = pr.ReviewDecision
			st.Author = pr.Author.Login
			st.State = pr.State
			if pr.Merged {
				st.State = "MERGED"
			}
			for _, rv := range pr.Reviews.Nodes {
				st.Reviews[rv.ID] = &Review{
					ID: rv.ID, Author: rv.Author.Login, IsBot: isBot(rv.Author),
					State: rv.State, SubmittedAt: rv.SubmittedAt, Body: rv.Body,
				}
			}
			if pr.Comments.PageInfo.HasNextPage {
				// More than 100 top-level comments. Same rule as an
				// over-full thread: mark the scan partial rather than
				// silently truncate, which would make an unread comment
				// look deleted next round.
				st.Complete = false
			}
			for _, cn := range pr.Comments.Nodes {
				c := &Comment{
					ID: cn.ID, URL: cn.URL, Body: cn.Body,
					CreatedAt: cn.CreatedAt, UpdatedAt: cn.UpdatedAt,
					Author: cn.Author.Login, IsBot: isBot(cn.Author),
				}
				c.computeDigest()
				st.Comments[c.ID] = c
			}
		}

		for _, tn := range pr.ReviewThreads.Nodes {
			t := &Thread{ID: tn.ID, Path: tn.Path, Line: tn.Line, IsResolved: tn.IsResolved, IsOutdated: tn.IsOutdated}
			if tn.Comments.PageInfo.HasNextPage {
				// More than 100 comments on one thread. Rather than
				// silently truncate — which would make every unread
				// comment look deleted — mark the scan partial.
				st.Complete = false
			}
			for _, cn := range tn.Comments.Nodes {
				c := &Comment{
					ID: cn.ID, URL: cn.URL, Body: cn.Body,
					CreatedAt: cn.CreatedAt, UpdatedAt: cn.UpdatedAt,
					Author: cn.Author.Login, IsBot: isBot(cn.Author),
				}
				c.computeDigest()
				t.Comments = append(t.Comments, c)
			}
			st.Threads[t.ID] = t
		}

		if !pr.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor = pr.ReviewThreads.PageInfo.EndCursor
	}
	return st, nil
}

// ListPRs finds pull requests matching a filter, for `ebac discover`.
func (g *GH) ListPRs(ctx context.Context, repo, author, state string, limit int) ([]PRRef, error) {
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
	refs := make([]PRRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, PRRef{
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
	if a.Typename == "Bot" || a.Typename == "EnterpriseUserAccount" && false {
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

func (r PRRef) Slug() string { return fmt.Sprintf("%s/%s", r.Owner, r.Repo) }

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
