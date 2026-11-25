package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// comment-edit (#64b): one edit of one of OUR comments, outside the cache.
// The cache cannot carry it: a merged PR's entity retires, and an edit on an
// ended form is reverted with an alarm (#71s). The checks are the cache's
// (comment_edit.go): the github-cache critic's trusted write authority
// (AuthorizeGHWrite, comment_edit), the comment written by the ours login and
// by this account (viewerDidAuthor), the disclose list, and the body GitHub
// holds right before the write equal to the one the operator saw (--expect,
// the first 12 hex of its sha256). After the write the comment is read again
// and must hold the new body. No form is written; nothing is tracked.

// commentGH is the forge the edit reads and writes through (forge.GH).
type commentGH interface {
	GraphQL(ctx context.Context, query string) (json.RawMessage, error)
	Mutate(ctx context.Context, mutation string, vars map[string]string) (json.RawMessage, error)
}

var commentNodeID = regexp.MustCompile(`^(IC|PRRC)_[A-Za-z0-9_-]{1,96}$`)

// bodyHash is the --expect value of a body (the cache's hash12).
func bodyHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// liveComment is one comment as GitHub has it now.
type liveComment struct {
	Review     bool
	Body       string
	Mine       bool // viewerDidAuthor
	Author     string
	Kind       string // pr | issue: what the comment is on
	N          int
	OnAuthor   string // that PR's or issue's author
	EntityRepo string
}

func commentQuery(id string) string {
	const parent = `number author{login} repository{nameWithOwner}`
	return fmt.Sprintf(`{node(id:%q){__typename `+
		`... on IssueComment{body viewerDidAuthor author{login} issue{%s} pullRequest{%s}} `+
		`... on PullRequestReviewComment{body viewerDidAuthor author{login} pullRequest{%s}}}}`,
		id, parent, parent, parent)
}

func readComment(ctx context.Context, gh commentGH, id string) (*liveComment, error) {
	data, err := gh.GraphQL(ctx, commentQuery(id))
	if err != nil {
		return nil, err
	}
	type parent struct {
		Number int
		Author *struct{ Login string }
		Repo   struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	var raw struct {
		Node *struct {
			Type   string `json:"__typename"`
			Body   string `json:"body"`
			Mine   bool   `json:"viewerDidAuthor"`
			Author *struct{ Login string }
			Issue  *parent `json:"issue"`
			PR     *parent `json:"pullRequest"`
		} `json:"node"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	n := raw.Node
	if n == nil || (n.Type != "IssueComment" && n.Type != "PullRequestReviewComment") {
		return nil, fmt.Errorf("%s is not a comment GitHub has (deleted, or another kind of node)", id)
	}
	c := &liveComment{Review: n.Type == "PullRequestReviewComment", Body: n.Body, Mine: n.Mine}
	if n.Author != nil {
		c.Author = n.Author.Login
	}
	p := n.PR // a conversation comment on a PR carries both: the PR wins
	c.Kind = "pr"
	if p == nil {
		p, c.Kind = n.Issue, "issue"
	}
	if p == nil {
		return nil, fmt.Errorf("%s: GitHub names no issue or pull request it is on", id)
	}
	c.N, c.EntityRepo = p.Number, p.Repo.NameWithOwner
	if p.Author != nil {
		c.OnAuthor = p.Author.Login
	}
	return c, nil
}

// commentEditReq is one edit as the operator asked it.
type commentEditReq struct {
	Repo, ID, Body, Expect string
	DryRun                 bool
}

// commentEdit runs the checks, writes and reads back. It returns the line to
// print; an error is a refusal or a failure (nothing written, or a read-back
// that does not hold the body).
func commentEdit(ctx context.Context, gh commentGH, w *model.Critic, r commentEditReq) (string, error) {
	if !commentNodeID.MatchString(r.ID) {
		return "", fmt.Errorf("refused: --comment %q is not a comment node id (IC_... or PRRC_...)", r.ID)
	}
	if strings.TrimSpace(r.Body) == "" && !r.DryRun {
		return "", fmt.Errorf("refused: an empty body")
	}
	ours := ""
	if w.GHWrite != nil {
		ours = w.GHWrite.Ours
	}
	if ours == "" {
		return "", fmt.Errorf("refused: critic %s's trusted scope names no ours login: no comment is ours", w.Name)
	}
	c, err := readComment(ctx, gh, r.ID)
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s#%d", c.EntityRepo, c.N)
	switch {
	case !strings.EqualFold(c.EntityRepo, r.Repo):
		return "", fmt.Errorf("refused: %s is in %s, not %s", r.ID, c.EntityRepo, r.Repo)
	case !strings.EqualFold(c.Author, ours):
		return "", fmt.Errorf("refused: %s on %s is %s's, not ours (%s): another author's text is never edited", r.ID, ref, model.FirstNonEmpty(c.Author, "an unknown author"), ours)
	case !c.Mine:
		return "", fmt.Errorf("refused: GitHub says this account did not write %s (viewerDidAuthor false)", r.ID)
	}
	if why := model.AuthorizeGHWrite(w, model.GHEntity{Repo: c.EntityRepo, Kind: c.Kind, N: c.N, Author: c.OnAuthor}, model.FieldCommentEdit); why != "" {
		return "", fmt.Errorf("refused: %s", why)
	}
	cur := bodyHash(c.Body)
	if r.DryRun {
		return fmt.Sprintf("%s on %s: current body --expect %s (%d B); nothing written", r.ID, ref, cur, len(c.Body)), nil
	}
	if why := disclose.Disclosure(r.Body); why != "" {
		return "", fmt.Errorf("refused: the new %s", why)
	}
	if r.Expect == "" || r.Expect != cur {
		return "", fmt.Errorf("refused: %s moved: GitHub's body is %s, --expect %q (read it again with --dry-run)", r.ID, cur, r.Expect)
	}
	if c.Body == r.Body {
		return fmt.Sprintf("%s on %s: GitHub already holds the body; nothing written", r.ID, ref), nil
	}
	if _, err := gh.Mutate(ctx, forge.EditCommentMutation(c.Review), map[string]string{"c": r.ID, "b": r.Body}); err != nil {
		return "", fmt.Errorf("the edit of %s failed: %w", r.ID, err)
	}
	back, err := readComment(ctx, gh, r.ID)
	if err != nil {
		return "", fmt.Errorf("edited %s, but the read-back failed: %w", r.ID, err)
	}
	if back.Body != r.Body {
		return "", fmt.Errorf("edited %s, but the read-back holds %s, not the body sent (%s)", r.ID, bodyHash(back.Body), bodyHash(r.Body))
	}
	return fmt.Sprintf("%s on %s: edited %s -> %s, read back", r.ID, ref, cur, bodyHash(back.Body)), nil
}

func cmdCommentEdit(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("comment-edit", flag.ExitOnError)
	name := fs.String("critic", "gh-cache", "the github-cache critic whose trusted write authority applies")
	repo := fs.String("repo", "", "owner/name the comment is in")
	id := fs.String("comment", "", "the comment's node id (IC_... or PRRC_...)")
	bodyFile := fs.String("body-file", "", "the new body: - for stdin (the only form through the sudo door), or a path")
	expect := fs.String("expect", "", "the current body's hash, as --dry-run prints it: the edit is refused if GitHub's body moved")
	dry := fs.Bool("dry-run", false, "read and check only; print the current body's --expect value")
	_ = fs.Parse(args)
	if *repo == "" || *id == "" {
		return fmt.Errorf("--repo and --comment are required")
	}
	var body []byte
	var err error
	switch *bodyFile {
	case "":
		if !*dry {
			return fmt.Errorf("--body-file is required (or --dry-run)")
		}
	case "-":
		body, err = io.ReadAll(os.Stdin)
	default:
		body, err = os.ReadFile(*bodyFile)
	}
	if err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	out, err := commentEdit(ctx, forge.NewGH(g.gh, model.FirstNonEmpty(w.Host, g.host)), w,
		commentEditReq{Repo: *repo, ID: *id, Body: string(body), Expect: *expect, DryRun: *dry})
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}
