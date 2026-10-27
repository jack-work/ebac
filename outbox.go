package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
)

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
func authorize(w *model.Critic, snap *model.Snapshot, me string, p *model.Post) string {
	if !w.Write && w.Mode != model.ModeReviewer {
		return "this critic is read-only: posting needs `ebac set-write --on`"
	}
	if p.React != "" {
		if p.ReplyTo == "" {
			return "react needs reply_to: the thread whose opening comment gets the reaction"
		}
		if _, ok := forge.Reactions[p.React]; !ok {
			return fmt.Sprintf("reaction %q: use one of +1 -1 laugh hooray confused heart rocket eyes", p.React)
		}
	}
	if p.Resolve {
		if p.ReplyTo == "" {
			return "resolve needs reply_to: the thread to resolve"
		}
		if w.Mode == model.ModeReviewer {
			return "a reviewer seat never resolves threads: the author does"
		}
	}
	if !p.Resolve && p.React == "" && !p.HasBody() {
		return "empty body"
	}
	var pr *model.PRState
	if snap != nil {
		pr = snap.PRs[model.PRKey(p.PR)]
	}
	if pr == nil {
		return fmt.Sprintf("%q is not a pull request of this critic", p.PR)
	}
	if len(pr.Pending) > 0 && (p.Event != "" || p.Path != "" || (p.ReplyTo != "" && p.HasBody())) {
		r := pr.Pending[0]
		return fmt.Sprintf("pending review %d by %s on %s (opened %s, %d comments only its author can see) would swallow a reply and refuses new reviews: "+
			"the operator must submit or delete it; a top-level comment (no --reply-to, --path or --event), --react and --resolve still work",
			r.DatabaseID, r.Author, p.PR, r.CreatedAt, r.Comments)
	}
	switch p.Event {
	case "", "COMMENT":
	case "APPROVE":
		if !w.Approve || w.Mode != model.ModeReviewer {
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

// sendPost takes one post as far as it can go this round.
func (p *Poller) sendPost(ctx context.Context, w *model.Critic, snap *model.Snapshot, id string, post *model.Post) {
	if why := authorize(w, snap, p.Me, post); why != "" {
		post.Refused = why
		return
	}
	pr := snap.PRs[model.PRKey(post.PR)]
	now := time.Now().UTC().Format(time.RFC3339)
	tried := false
	defer func() {
		if tried {
			post.Attempts++
		}
	}()
	if post.HasBody() && post.Sent == nil {
		marker := postMarker(w.Name, id)
		if url, ok := model.FindMarker(pr, p.Me, marker); ok {
			post.Sent, post.Error = &model.PostReceipt{URL: url, At: now, Recovered: true}, ""
		} else {
			tried = true
			url, err := p.GH.Post(ctx, pr, post, post.Body+"\n\n"+marker)
			var pend *forge.ErrPendingReply
			if errors.As(err, &pend) {
				post.Refused = fmt.Sprintf("landed in pending review %d, visible only to its author; not sent and not retried (a retry would add another hidden comment): the operator must submit or delete that review", pend.ReviewID)
				return
			}
			if why := forge.ClientError(err); why != "" {
				post.Refused = "the forge refused it: " + why
				return
			}
			if err != nil {
				post.Error = err.Error()
				return
			}
			post.Sent, post.Error = &model.PostReceipt{URL: url, At: now}, ""
		}
	}
	if post.React != "" && post.Reacted == "" {
		th := pr.Threads[post.ReplyTo]
		first := th.FirstComment()
		if first == nil {
			post.Error = "thread " + post.ReplyTo + " has no comment to react to"
			return
		}
		tried = true
		if err := p.GH.React(ctx, first.ID, forge.Reactions[post.React]); err != nil {
			if why := forge.ClientError(err); why != "" {
				post.Refused = "the forge refused the reaction: " + why
				return
			}
			post.Error = err.Error()
			return
		}
		post.Reacted, post.Error = now, ""
	}
	if post.Resolve && post.Resolved == "" {
		if th := pr.Threads[post.ReplyTo]; th != nil && th.IsResolved {
			post.Resolved, post.Error = now, ""
			return
		}
		tried = true
		if err := p.GH.ResolveThread(ctx, post.ReplyTo); err != nil {
			if why := forge.ClientError(err); why != "" {
				post.Refused = "the forge refused the resolve: " + why
				return
			}
			post.Error = err.Error()
			return
		}
		post.Resolved, post.Error = now, ""
	}
}

// flushOutbox posts what the seat wrote since the watermark and returns the
// receipts for the round's one patch, and moves the watermark
// in w. On any read failure it moves nothing.
func (p *Poller) flushOutbox(ctx context.Context, w *model.Critic, snap *model.Snapshot) Receipts {
	acks := Receipts{}
	c, err := p.Fig.Node(ctx, w.FormID)
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
	var posts map[string]*model.Post
	if err := json.Unmarshal(model.MustJSON(cur["post"]), &posts); err != nil {
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
		if post == nil || post.Terminal() {
			continue
		}
		hadError := post.Error != ""
		if err := validPostID(id); err != nil {
			post.Refused = err.Error()
		} else {
			p.sendPost(ctx, w, snap, id, post)
		}
		r := map[string]json.RawMessage{}
		if post.Sent != nil {
			r["sent"] = model.MustJSON(post.Sent)
		}
		if post.Resolved != "" {
			r["resolved"] = model.MustJSON(post.Resolved)
		}
		if post.Reacted != "" {
			r["reacted"] = model.MustJSON(post.Reacted)
		}
		if post.Refused != "" {
			r["refused"] = model.MustJSON(post.Refused)
		}
		if post.Error != "" || hadError {
			r["error"] = model.MustJSON(post.Error)
		}
		if post.Attempts > 0 {
			r["attempts"] = model.MustJSON(post.Attempts)
		}
		if post.Terminal() {
			r["done_at"] = model.MustJSON(time.Now().UTC().Format(time.RFC3339))
		}
		acks[id] = r
		switch {
		case post.Refused != "":
			fmt.Fprintf(p.Out, "%s: refused %s: %s\n", w.Name, id, post.Refused)
		case post.Error != "":
			fmt.Fprintf(p.Out, "%s: post %s failed (attempt %d): %s\n", w.Name, id, post.Attempts, post.Error)
		default:
			url := "(no reply)"
			if post.Sent != nil {
				url = post.Sent.URL
			}
			res := ""
			if post.Resolved != "" {
				res = ", thread resolved"
			}
			fmt.Fprintf(p.Out, "%s: posted %s -> %s%s\n", w.Name, id, url, res)
		}
	}
	w.Runtime.OutboxWatermark = since.Version
	return acks
}
