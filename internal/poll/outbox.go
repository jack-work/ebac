package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// validPostID keeps an id usable as one form path segment.
func ValidPostID(id string) error {
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

func NewPostID(now time.Time) string {
	return "p" + strconv.FormatInt(now.UnixNano(), 36)
}

// authorize says why a post may not go out, or "" if it may.
func Authorize(w *model.Critic, snap *model.Snapshot, me string, p *model.Post) string {
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

// FlushOutbox posts what the seat wrote since the watermark and returns the
// receipts for the round's one patch, and moves the watermark in w. On any
// read failure it moves nothing. The core authorizes every post; only
// authorized posts reach the plugin (plugin.OpPost), which takes them as far
// as they go upstream and returns their receipt fields.
func (p *Poller) FlushOutbox(ctx context.Context, w *model.Critic, snap *model.Snapshot) form.Receipts {
	acks := form.Receipts{}
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

	todo := map[string]*model.Post{}
	hadError := map[string]bool{}
	var order []string
	for _, id := range sorted {
		post := posts[id]
		if post == nil || post.Terminal() {
			continue
		}
		hadError[id] = post.Error != ""
		if err := ValidPostID(id); err != nil {
			post.Refused = err.Error()
		} else if why := Authorize(w, snap, p.Me, post); why != "" {
			post.Refused = why
		} else {
			todo[id] = post
		}
		order = append(order, id)
	}
	p.post(ctx, w, snap, todo)
	for _, id := range order {
		post := posts[id]
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
		if post.Error != "" || hadError[id] {
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

// post hands authorized posts to the source's plugin and merges back what it
// did. A plugin failure is every post's error for this round: each is retried
// on the next.
func (p *Poller) post(ctx context.Context, w *model.Critic, snap *model.Snapshot, todo map[string]*model.Post) {
	if len(todo) == 0 {
		return
	}
	rep, err := p.call(ctx, w, plugin.Request{Op: plugin.OpPost, Snapshot: snap, Posts: todo})
	if err != nil {
		for _, post := range todo {
			post.Error = err.Error()
		}
		return
	}
	for _, l := range rep.Log {
		fmt.Fprintln(p.Out, l)
	}
	for id, post := range todo {
		if got := rep.Posts[id]; got != nil {
			*post = *got
		} else {
			post.Error = "the plugin returned no result for this post"
		}
	}
}
