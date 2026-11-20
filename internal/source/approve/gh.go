package approve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gluck/ebac/sdk/model"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/forge"
)

// GH is this source's whole GHE surface: two reads and one APPROVE, on Repo
// only. It is deliberately not forge.GH (which can comment, push and merge):
// nothing else is reachable from the approve-requests source. Every call is
// argv to the gh binary, never a shell.
type GH struct {
	Bin     string
	Cred    *forge.Cred // band-cred ghe-ebac; nil only in tests
	Timeout time.Duration
	// exec runs gh with argv args and returns stdout. nil is the real gh;
	// tests replace it to record every argv.
	exec  func(ctx context.Context, args []string) ([]byte, error)
	spend Spend
}

// PRInfo is what scope needs of a pull request.
type PRInfo struct {
	State string `json:"state"` // open, closed
	Draft bool   `json:"draft"`
	Head  struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// Review is one pull request review.
type Review struct {
	ID          int64  `json:"id"`
	State       string `json:"state"` // APPROVED, COMMENTED, ...
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
		Type  string `json:"type"` // User, Bot
	} `json:"user"`
}

// PR reads one pull request of Repo (GET).
func (g *GH) PR(ctx context.Context, n int) (PRInfo, error) {
	var p PRInfo
	out, err := g.run(ctx, []string{"api", "repos/" + Repo + "/pulls/" + strconv.Itoa(n)})
	if err == nil {
		err = json.Unmarshal(out, &p)
	}
	return p, err
}

// Reviews reads a pull request's reviews (GET, first 100).
func (g *GH) Reviews(ctx context.Context, n int) ([]Review, error) {
	var rs []Review
	out, err := g.run(ctx, []string{"api", "repos/" + Repo + "/pulls/" + strconv.Itoa(n) + "/reviews?per_page=100"})
	if err == nil {
		err = json.Unmarshal(out, &rs)
	}
	return rs, err
}

// Approve posts an APPROVE review on a pull request of Repo: the one write.
func (g *GH) Approve(ctx context.Context, n int, body string) error {
	_, err := g.run(ctx, []string{"pr", "review", strconv.Itoa(n), "--repo", Repo, "--approve", "--body", body})
	return err
}

func (g *GH) run(ctx context.Context, args []string) ([]byte, error) {
	if g.exec != nil {
		return g.exec(ctx, args)
	}
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	env := append(os.Environ(), "GH_HOST="+Host, "GH_PAGER=", "NO_COLOR=1", "GH_PROMPT_DISABLED=1")
	if g.Cred != nil {
		tok, err := g.Cred.Token(ctx)
		if err != nil {
			return nil, err
		}
		env = append(env, "GH_TOKEN="+tok, "GH_ENTERPRISE_TOKEN="+tok)
	}
	cmd := forge.ExecCommandContext(ctx, g.Bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("gh %s: %s", args[0], msg)
	}
	return out.Bytes(), nil
}

// Spend is what this GH has spent on GraphQL reads (points are charged per
// call, min 1; every caller on the account shares one 5000/h bucket).
type Spend struct {
	Calls, Points int
	Remaining     int // after the last call; -1 when none answered
}

func (s Spend) String() string {
	left := "?"
	if s.Remaining >= 0 {
		left = strconv.Itoa(s.Remaining)
	}
	return fmt.Sprintf("graphql %d call(s), %d point(s), %s left", s.Calls, s.Points, left)
}

// Budget is the spend in the budget ledger's terms (GraphQL points; the
// ledger assumes the hour window for the reported remaining).
func (s Spend) Budget() []model.Spend {
	if s.Calls == 0 {
		return nil
	}
	return []model.Spend{{Limit: "graphql", N: float64(s.Points), Remaining: s.Remaining}}
}

// Spent is the GraphQL spend so far.
func (g *GH) Spent() Spend {
	s := g.spend
	if s.Calls == 0 {
		s.Remaining = -1
	}
	return s
}

// Query runs one GraphQL READ (a query, never a mutation) and returns its
// data without rateLimit, which it asks so each round can log its cost.
func (g *GH) Query(ctx context.Context, q string) (json.RawMessage, error) {
	q = strings.TrimSpace(q)
	if !strings.HasPrefix(q, "query{") || strings.Contains(strings.ToLower(q), "mutation") {
		return nil, fmt.Errorf("graphql: only a read (query{...}) is allowed")
	}
	q = "query{ rateLimit{cost remaining} " + q[len("query{"):]
	out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + q})
	if err != nil {
		return nil, err
	}
	var r struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode graphql: %w", err)
	}
	g.spend.Calls++
	var rl struct {
		Cost      int `json:"cost"`
		Remaining int `json:"remaining"`
	}
	if raw, ok := r.Data["rateLimit"]; ok && json.Unmarshal(raw, &rl) == nil {
		g.spend.Points += rl.Cost
		g.spend.Remaining = rl.Remaining
	} else {
		g.spend.Points++
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", oneLine(r.Errors[0].Message))
	}
	delete(r.Data, "rateLimit")
	return json.Marshal(r.Data)
}
