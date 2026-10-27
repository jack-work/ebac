package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// Post sends one post to the forge and returns where it landed.
func (g *GH) Post(ctx context.Context, pr *model.PRState, p *model.Post, body string) (string, error) {
	g.pace()
	switch {
	case p.ReplyTo != "":
		q := `mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{url state pullRequestReview{databaseId}}}}`
		out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + q, "-f", "t=" + p.ReplyTo, "-f", "b=" + body,
			"-q", ".data.addPullRequestReviewThreadReply.comment"}, nil)
		if err != nil {
			return "", err
		}
		var c struct {
			URL    string `json:"url"`
			State  string `json:"state"`
			Review struct {
				DatabaseID int64 `json:"databaseId"`
			} `json:"pullRequestReview"`
		}
		if err := json.Unmarshal(out, &c); err != nil {
			return "", fmt.Errorf("reply: unreadable answer %q: %w", strings.TrimSpace(string(out)), err)
		}
		if c.State != "SUBMITTED" {
			// PENDING: absorbed into an unsubmitted review. Anything else is
			// unknown; neither is a published reply.
			return "", &ErrPendingReply{ReviewID: c.Review.DatabaseID, State: c.State, URL: c.URL}
		}
		return c.URL, nil
	case p.Path != "" || p.Event != "":
		review := map[string]any{"commit_id": pr.HeadSHA, "event": model.FirstNonEmpty(p.Event, "COMMENT")}
		if p.Path != "" {
			review["comments"] = []map[string]any{{"path": p.Path, "line": p.Line, "side": "RIGHT", "body": body}}
		} else {
			review["body"] = body
		}
		in := model.LiteralJSON(review)
		out, err := g.run(ctx, []string{"api", fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", pr.Owner, pr.Repo, pr.Number),
			"-X", "POST", "--input", "-", "-q", ".html_url"}, in)
		return strings.TrimSpace(string(out)), err
	default:
		in := model.LiteralJSON(map[string]string{"body": body})
		out, err := g.run(ctx, []string{"api", fmt.Sprintf("repos/%s/%s/issues/%d/comments", pr.Owner, pr.Repo, pr.Number),
			"-X", "POST", "--input", "-", "-q", ".html_url"}, in)
		return strings.TrimSpace(string(out)), err
	}
}

// React adds a reaction. A second identical reaction by the same user is a
// no-op on the forge, so a retry is safe.
func (g *GH) React(ctx context.Context, subject, content string) error {
	g.pace()
	q := `mutation($s:ID!,$c:ReactionContent!){addReaction(input:{subjectId:$s,content:$c}){reaction{content}}}`
	out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + q, "-f", "s=" + subject, "-f", "c=" + content,
		"-q", ".data.addReaction.reaction.content"}, nil)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != content {
		return fmt.Errorf("react %s: forge answered %q", subject, got)
	}
	return nil
}

// ResolveThread resolves a review thread. Resolving a resolved thread is a
// no-op on the forge, so a retry is safe.
func (g *GH) ResolveThread(ctx context.Context, thread string) error {
	g.pace()
	q := `mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}`
	out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + q, "-f", "t=" + thread,
		"-q", ".data.resolveReviewThread.thread.isResolved"}, nil)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		return fmt.Errorf("resolve %s: forge answered %q", thread, got)
	}
	return nil
}

// reactions maps the names a seat writes to GitHub's ReactionContent.
var Reactions = map[string]string{
	"+1": "THUMBS_UP", "-1": "THUMBS_DOWN", "laugh": "LAUGH", "hooray": "HOORAY",
	"confused": "CONFUSED", "heart": "HEART", "rocket": "ROCKET", "eyes": "EYES",
}

// errPendingReply: the forge took the reply but did not publish it.
type ErrPendingReply struct {
	ReviewID int64
	State    string
	URL      string
}

func (e *ErrPendingReply) Error() string {
	return fmt.Sprintf("reply landed as %s in review %d (%s)", e.State, e.ReviewID, e.URL)
}

var httpStatus = regexp.MustCompile(`\(HTTP (\d{3})\)|HTTP (\d{3})`)

// clientError names a 4xx refusal, which no retry will change. 429 (rate
// limited) is the exception: it is transient.
func ClientError(err error) string {
	if err == nil {
		return ""
	}
	m := httpStatus.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	code := m[1] + m[2]
	if code[0] != '4' || code == "429" {
		return ""
	}
	return err.Error()
}
