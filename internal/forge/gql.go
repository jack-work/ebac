package forge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// GraphQL points are charged per CALL (min 1), not per node, and every gh
// caller on the account shares one 5000 points/h bucket (2026-11-17: it ran
// dry). So every read here is one aliased query for many PRs, and every query
// asks rateLimit{cost remaining} so a round can log what it spent.

// Spend is what this process has spent on GraphQL reads.
type Spend struct {
	Calls     int
	Points    int
	Remaining int // after the last call; -1 when no call has answered
}

func (s Spend) String() string {
	left := "?"
	if s.Remaining >= 0 {
		left = strconv.Itoa(s.Remaining)
	}
	return fmt.Sprintf("graphql %d call(s), %d point(s), %s left", s.Calls, s.Points, left)
}

// Spent is the GraphQL spend so far.
func (g *GH) Spent() Spend {
	g.spendMu.Lock()
	defer g.spendMu.Unlock()
	s := g.spend
	if s.Calls == 0 {
		s.Remaining = -1
	}
	return s
}

type gqlError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Path    []any  `json:"path"`
}

// withRateLimit adds rateLimit{cost remaining} to a query's top selection.
func withRateLimit(q string) string {
	i := strings.IndexByte(q, '{')
	if i < 0 || strings.Contains(q, "rateLimit") {
		return q
	}
	return q[:i+1] + " rateLimit{cost remaining} " + q[i+1:]
}

// query runs one GraphQL read, counts its cost and returns its data without
// the rateLimit key, and the errors GitHub returned beside it (an aliased
// query answers the aliases it could and names the others in errors).
func (g *GH) query(ctx context.Context, q string) (json.RawMessage, []gqlError, error) {
	out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + withRateLimit(q)}, nil)
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []gqlError                 `json:"errors"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, nil, fmt.Errorf("decode graphql: %w", err)
	}
	g.spendMu.Lock()
	g.spend.Calls++
	var rl struct {
		Cost      int `json:"cost"`
		Remaining int `json:"remaining"`
	}
	if raw, ok := r.Data["rateLimit"]; ok && json.Unmarshal(raw, &rl) == nil {
		g.spend.Points += rl.Cost
		g.spend.Remaining = rl.Remaining
	} else {
		g.spend.Points++ // the minimum a call costs
	}
	g.spendMu.Unlock()
	delete(r.Data, "rateLimit")
	if r.Data == nil {
		return nil, r.Errors, nil
	}
	data, err := json.Marshal(r.Data)
	return data, r.Errors, err
}

// PRBatch bounds how many PRs one deep read asks: each carries up to 50
// threads of 100 comments (10 PRs measured 5 points).
const PRBatch = 10

// prSel is everything one round needs about one PR. reviews and reviewThreads
// are BOTH paginated with their own cursor; the caller honours each
// hasNextPage or marks the snapshot incomplete. reviews used to be
// `reviews(last:30)` with no cursor at all: on bic/aether#24485 (over a
// hundred review submissions) every review past the newest 30 was silently
// dropped, a zero that meant "the instrument didn't look".
func prSel(threadsAfter, reviewsAfter string) string {
	after := func(c string) string {
		if c == "" {
			return ""
		}
		return ", after:" + strconv.Quote(c)
	}
	return `number url title state isDraft merged reviewDecision updatedAt baseRefName
      author{login __typename}
      headRefOid
      pending: reviews(states:PENDING, first:5){
        nodes{ id databaseId createdAt author{login} comments{ totalCount } }
      }
      reviews(first:50` + after(reviewsAfter) + `){
        pageInfo{ hasNextPage endCursor }
        nodes{ id state submittedAt body author{login __typename} }
      }
      comments(first:100){
        pageInfo{ hasNextPage }
        nodes{ id url body createdAt updatedAt author{login __typename} }
      }
      reviewThreads(first:50` + after(threadsAfter) + `){
        pageInfo{ hasNextPage endCursor }
        nodes{
          id isResolved isOutdated path line
          comments(first:100){
            pageInfo{ hasNextPage }
            nodes{ id url body state createdAt updatedAt author{login __typename} }
          }
        }
      }`
}

// probeSel is the cheap read of one PR: what moves when anything a critic
// diffs moves, except an edit to a comment body (the periodic full read
// catches those). 10 PRs in one query measured 1 point.
const probeSel = `updatedAt headRefOid state merged isDraft reviewDecision baseRefName title
      pending: reviews(states:PENDING, first:5){ nodes{ id comments{ totalCount } } }
      reviews{ totalCount }
      comments{ totalCount }
      reviewThreads(first:100){ totalCount nodes{ id isResolved isOutdated comments{ totalCount } } }`

// aliasedPRs is one query reading each PR under alias p<i>.
func aliasedPRs(refs []model.PRRef, sel func(i int) string) string {
	var b strings.Builder
	b.WriteString("query{")
	for i, r := range refs {
		fmt.Fprintf(&b, "\n  p%d: repository(owner:%s,name:%s){ pullRequest(number:%d){ %s } }",
			i, strconv.Quote(r.Owner), strconv.Quote(r.Repo), r.Number, sel(i))
	}
	b.WriteString("\n}")
	return b.String()
}

// batch runs one aliased query and splits it per alias: each alias's
// pullRequest object, or its own error. A call that failed outright (rate
// limit, timeout) is the error for every alias.
func (g *GH) batch(ctx context.Context, refs []model.PRRef, sel func(i int) string) ([]json.RawMessage, []error) {
	prs := make([]json.RawMessage, len(refs))
	errs := make([]error, len(refs))
	data, gerrs, err := g.query(ctx, aliasedPRs(refs, sel))
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return prs, errs
	}
	var whole error
	for _, e := range gerrs {
		if len(e.Path) > 0 {
			if a, ok := e.Path[0].(string); ok && strings.HasPrefix(a, "p") {
				if i, perr := strconv.Atoi(a[1:]); perr == nil && i < len(refs) {
					if errs[i] == nil {
						errs[i] = fmt.Errorf("graphql: %s", e.Message)
					}
					continue
				}
			}
		}
		if whole == nil {
			whole = fmt.Errorf("graphql: %s", e.Message)
		}
	}
	var byAlias map[string]struct {
		PullRequest json.RawMessage `json:"pullRequest"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &byAlias); err != nil {
			whole = fmt.Errorf("decode graphql: %w", err)
		}
	}
	for i, r := range refs {
		if errs[i] != nil {
			continue
		}
		pr := byAlias[fmt.Sprintf("p%d", i)].PullRequest
		switch {
		case len(pr) > 0 && string(pr) != "null":
			prs[i] = pr
		case whole != nil:
			errs[i] = whole
		default:
			errs[i] = fmt.Errorf("%s/%s#%d not found", r.Owner, r.Repo, r.Number)
		}
	}
	return prs, errs
}

// FetchPR returns one PR's state (FetchPRs of one).
func (g *GH) FetchPR(ctx context.Context, owner, repo string, number int) (*model.PRState, error) {
	sts, errs := g.FetchPRs(ctx, []model.PRRef{{Owner: owner, Repo: repo, Number: number}})
	return sts[0], errs[0]
}

// FetchPRs reads every PR in refs, PRBatch to a query, each later page of the
// PRs that have one in one more aliased query. A returned PRState carries
// Complete=false if any page was left unread, which forbids the differ from
// inferring deletions for it. sts[i] is nil exactly when errs[i] is set.
func (g *GH) FetchPRs(ctx context.Context, refs []model.PRRef) ([]*model.PRState, []error) {
	sts := make([]*model.PRState, len(refs))
	errs := make([]error, len(refs))
	type cursor struct{ threads, reviews string }
	cur := make([]cursor, len(refs))
	todo := make([]int, 0, len(refs))
	for i, r := range refs {
		sts[i] = &model.PRState{
			Key: model.MakePRKey(r.Owner, r.Repo, r.Number), Owner: r.Owner, Repo: r.Repo, Number: r.Number,
			Threads: map[string]*model.Thread{}, Reviews: map[string]*model.Review{}, Comments: map[string]*model.Comment{}, Complete: true,
		}
		todo = append(todo, i)
	}
	for page := 0; len(todo) > 0; page++ {
		if page > 40 {
			for _, i := range todo {
				sts[i].Complete = false
			}
			break
		}
		var next []int
		for lo := 0; lo < len(todo); lo += PRBatch {
			part := todo[lo:min(lo+PRBatch, len(todo))]
			asked := make([]model.PRRef, len(part))
			for k, i := range part {
				asked[k] = refs[i]
			}
			raws, berrs := g.batch(ctx, asked, func(k int) string { c := cur[part[k]]; return prSel(c.threads, c.reviews) })
			for k, i := range part {
				if berrs[k] != nil {
					errs[i] = berrs[k]
					continue
				}
				var pr gqlPR
				if err := json.Unmarshal(raws[k], &pr); err != nil {
					errs[i] = fmt.Errorf("decode graphql: %w", err)
					continue
				}
				applyPage(sts[i], &pr, page)
				threadsMore, reviewsMore := pr.ReviewThreads.PageInfo.HasNextPage, pr.Reviews.PageInfo.HasNextPage
				if threadsMore {
					cur[i].threads = pr.ReviewThreads.PageInfo.EndCursor
				}
				if reviewsMore {
					cur[i].reviews = pr.Reviews.PageInfo.EndCursor
				}
				if threadsMore || reviewsMore {
					next = append(next, i)
				}
			}
		}
		todo = next
	}
	for i := range refs {
		if errs[i] != nil {
			sts[i] = nil
			continue
		}
		MarkStamped(sts[i].Reviews)
	}
	return sts, errs
}

// applyPage folds one page of one PR into st. Reviews are paginated
// independently of reviewThreads: once the reviews cursor is exhausted, later
// pages (still walking reviewThreads) repeat its last, empty page, harmless.
// Pending is asked on every page and is the same each time.
func applyPage(st *model.PRState, pr *gqlPR, page int) {
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
			// More than 100 top-level comments: mark the scan partial rather
			// than silently truncate, which would make an unread comment look
			// deleted next round.
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
			// More than 100 comments on one thread: partial, never truncated.
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
}

// ProbePRs reads the cheap fingerprint of every PR in refs (50 to a query:
// one point for a critic's whole set). fps[i] is "" exactly when errs[i] is set.
func (g *GH) ProbePRs(ctx context.Context, refs []model.PRRef) ([]string, []error) {
	const per = 50
	fps := make([]string, len(refs))
	errs := make([]error, len(refs))
	for lo := 0; lo < len(refs); lo += per {
		part := refs[lo:min(lo+per, len(refs))]
		raws, berrs := g.batch(ctx, part, func(int) string { return probeSel })
		for k := range part {
			if berrs[k] != nil {
				errs[lo+k] = berrs[k]
				continue
			}
			fps[lo+k] = fingerprint(raws[k])
		}
	}
	return fps, errs
}

// fingerprint is a digest of a probe answer with its thread list in a stable
// order (GitHub's order is stable, but a digest must not depend on it).
func fingerprint(raw json.RawMessage) string {
	var p map[string]any
	if json.Unmarshal(raw, &p) != nil {
		return "undecodable:" + strconv.Itoa(len(raw))
	}
	if rt, ok := p["reviewThreads"].(map[string]any); ok {
		if nodes, ok := rt["nodes"].([]any); ok {
			sort.Slice(nodes, func(a, b int) bool {
				ia, _ := nodes[a].(map[string]any)["id"].(string)
				ib, _ := nodes[b].(map[string]any)["id"].(string)
				return ia < ib
			})
		}
	}
	b, _ := json.Marshal(p) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type gqlAuthor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

type gqlPR struct {
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
		Nodes []gqlComment `json:"nodes"`
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
				Nodes []gqlComment `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

type gqlComment struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	Author    gqlAuthor `json:"author"`
}
