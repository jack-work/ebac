package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The outbox: the review interface, through the form.
//
// A seat (or `ebac post`) writes post.<id> onto the critic's form. Each round
// the poller reads every patch since its watermark, posts what is new to the
// forge, and writes the receipt back onto the same post, in the same patch as
// the new watermark. A post that did not reach an end gets a receipt
// (attempts, error) too, and that receipt is itself a patch past the
// watermark, so the next round sees the post again: retry needs no other
// bookkeeping. If the round's patch is lost, neither moves. A hidden marker in
// every body makes a post that landed but was never acked (a crash, a second
// ebac) recognisable instead of posted twice.

const maxPostAttempts = 5

type Post struct {
	PR      string `json:"pr"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"` // review thread id: reply in that thread
	Path    string `json:"path,omitempty"`     // with Line: a new inline comment
	Line    int    `json:"line,omitempty"`
	Event   string `json:"event,omitempty"` // COMMENT | APPROVE: submit as a review

	Sent     *PostReceipt `json:"sent,omitempty"`
	Refused  string       `json:"refused,omitempty"`
	Error    string       `json:"error,omitempty"`
	Attempts int          `json:"attempts,omitempty"`
}

type PostReceipt struct {
	URL string `json:"url"`
	At  string `json:"at"`
	// Recovered: the marker was already on the forge, so nothing was posted.
	Recovered bool `json:"recovered,omitempty"`
}

func (p *Post) terminal() bool {
	return p.Sent != nil || p.Refused != "" || p.Attempts >= maxPostAttempts
}

func postMarker(critic, id string) string {
	return fmt.Sprintf("<!-- ebac:post:%s:%s -->", critic, id)
}

// validPostID keeps an id usable as one form path segment.
func validPostID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("post id %q: 1-64 characters", id)
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("post id %q: use only [A-Za-z0-9_-]", id)
		}
	}
	return nil
}

func newPostID(now time.Time) string {
	return "p" + strconv.FormatInt(now.UnixNano(), 36)
}

// authorize says why a post may not go out, or "" if it may.
func authorize(w *Critic, snap *Snapshot, me string, p *Post) string {
	if !w.Write && w.Mode != ModeReviewer {
		return "this critic is read-only: posting needs `ebac set-write --on`"
	}
	if strings.TrimSpace(p.Body) == "" {
		return "empty body"
	}
	var pr *PRState
	if snap != nil {
		pr = snap.PRs[PRKey(p.PR)]
	}
	if pr == nil {
		return fmt.Sprintf("%q is not a pull request of this critic", p.PR)
	}
	switch p.Event {
	case "", "COMMENT":
	case "APPROVE":
		if !w.Approve || w.Mode != ModeReviewer {
			return "approval is not granted on this critic"
		}
		if strings.EqualFold(pr.Author, me) {
			return "a seat may not approve its own operator's pull request"
		}
	default:
		return fmt.Sprintf("event %q is never allowed (only COMMENT or APPROVE)", p.Event)
	}
	if p.ReplyTo != "" {
		if _, ok := pr.Threads[p.ReplyTo]; !ok {
			return fmt.Sprintf("thread %s is not on %s", p.ReplyTo, p.PR)
		}
	}
	if (p.Path == "") != (p.Line == 0) {
		return "an inline comment needs both path and line"
	}
	if p.ReplyTo != "" && p.Path != "" {
		return "give reply_to or path/line, not both"
	}
	return ""
}

// findMarker returns the URL of a comment of ours already carrying marker.
func findMarker(pr *PRState, me, marker string) (string, bool) {
	mine := func(author, body string) bool {
		return strings.EqualFold(author, me) && strings.Contains(body, marker)
	}
	for _, t := range pr.Threads {
		for _, c := range t.Comments {
			if mine(c.Author, c.Body) {
				return c.URL, true
			}
		}
	}
	for _, c := range pr.Comments {
		if mine(c.Author, c.Body) {
			return c.URL, true
		}
	}
	for _, r := range pr.Reviews {
		if mine(r.Author, r.Body) {
			return pr.URL, true
		}
	}
	return "", false
}

// sendPost takes one post as far as it can go this round.
func (p *Poller) sendPost(ctx context.Context, w *Critic, snap *Snapshot, id string, post *Post) {
	if why := authorize(w, snap, p.Me, post); why != "" {
		post.Refused = why
		return
	}
	pr := snap.PRs[PRKey(post.PR)]
	marker := postMarker(w.Name, id)
	now := time.Now().UTC().Format(time.RFC3339)
	if url, ok := findMarker(pr, p.Me, marker); ok {
		post.Sent, post.Error = &PostReceipt{URL: url, At: now, Recovered: true}, ""
		return
	}
	url, err := p.GH.Post(ctx, pr, post, post.Body+"\n\n"+marker)
	post.Attempts++
	if err != nil {
		post.Error = err.Error()
		return
	}
	post.Sent, post.Error = &PostReceipt{URL: url, At: now}, ""
}

// flushOutbox posts what the seat wrote since the watermark and returns the
// receipts as form paths for the round's one patch, and moves the watermark
// in w. On any read failure it moves nothing.
func (p *Poller) flushOutbox(ctx context.Context, w *Critic, snap *Snapshot) map[string]json.RawMessage {
	acks := map[string]json.RawMessage{}
	c, err := p.Fig.node(ctx, w.FormID)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	since, err := c.FormSince(ctx, w.Runtime.OutboxWatermark)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	cur, err := p.Fig.Form(ctx, w.FormID)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	var posts map[string]*Post
	if err := json.Unmarshal(mustJSON(cur["post"]), &posts); err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unparseable: %v\n", w.Name, err)
		return acks
	}
	ids := map[string]bool{}
	if since.Delta {
		for _, vp := range since.Patches {
			for _, e := range vp.Patch.Entries() {
				if rest, ok := strings.CutPrefix(e.Key, "post."); ok {
					ids[strings.SplitN(rest, ".", 2)[0]] = true
				} else if e.Key == "post" {
					for id := range posts {
						ids[id] = true
					}
				}
			}
		}
	} else {
		for id := range posts {
			ids[id] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	for _, id := range sorted {
		post := posts[id]
		if post == nil || post.terminal() {
			continue
		}
		hadError := post.Error != ""
		if err := validPostID(id); err != nil {
			post.Refused = err.Error()
		} else {
			p.sendPost(ctx, w, snap, id, post)
		}
		if post.Sent != nil {
			acks["post."+id+".sent"] = mustJSON(post.Sent)
		}
		if post.Refused != "" {
			acks["post."+id+".refused"] = mustJSON(post.Refused)
		}
		if post.Error != "" || hadError {
			acks["post."+id+".error"] = mustJSON(post.Error)
		}
		if post.Attempts > 0 {
			acks["post."+id+".attempts"] = mustJSON(post.Attempts)
		}
		switch {
		case post.Sent != nil:
			fmt.Fprintf(p.Out, "%s: posted %s -> %s\n", w.Name, id, post.Sent.URL)
		case post.Refused != "":
			fmt.Fprintf(p.Out, "%s: refused %s: %s\n", w.Name, id, post.Refused)
		default:
			fmt.Fprintf(p.Out, "%s: post %s failed (attempt %d): %s\n", w.Name, id, post.Attempts, post.Error)
		}
	}
	w.Runtime.OutboxWatermark = since.Version
	return acks
}

// Post sends one post to the forge and returns where it landed.
func (g *GH) Post(ctx context.Context, pr *PRState, p *Post, body string) (string, error) {
	switch {
	case p.ReplyTo != "":
		q := `mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{url}}}`
		out, err := g.run(ctx, []string{"api", "graphql", "-f", "query=" + q, "-f", "t=" + p.ReplyTo, "-f", "b=" + body,
			"-q", ".data.addPullRequestReviewThreadReply.comment.url"}, nil)
		return strings.TrimSpace(string(out)), err
	case p.Path != "" || p.Event != "":
		review := map[string]any{"commit_id": pr.HeadSHA, "event": firstNonEmpty(p.Event, "COMMENT")}
		if p.Path != "" {
			review["comments"] = []map[string]any{{"path": p.Path, "line": p.Line, "side": "RIGHT", "body": body}}
		} else {
			review["body"] = body
		}
		in := literalJSON(review)
		out, err := g.run(ctx, []string{"api", fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", pr.Owner, pr.Repo, pr.Number),
			"-X", "POST", "--input", "-", "-q", ".html_url"}, in)
		return strings.TrimSpace(string(out)), err
	default:
		in := literalJSON(map[string]string{"body": body})
		out, err := g.run(ctx, []string{"api", fmt.Sprintf("repos/%s/%s/issues/%d/comments", pr.Owner, pr.Repo, pr.Number),
			"-X", "POST", "--input", "-", "-q", ".html_url"}, in)
		return strings.TrimSpace(string(out)), err
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// literalJSON encodes without HTML escaping, so a body's <!-- marker --> is
// sent as written.
func literalJSON(v any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return []byte(strings.TrimSpace(b.String()))
}
