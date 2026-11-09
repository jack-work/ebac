package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
)

// sendPost takes one post as far as it can go this round. The core has
// already authorized it (poll.Authorize); this only talks to the forge.
func sendPost(ctx context.Context, gh *forge.GH, w *model.Critic, snap *model.Snapshot, me, id string, post *model.Post) {
	if snap == nil || snap.PRs[model.PRKey(post.PR)] == nil {
		post.Error = "no state for " + post.PR + " this round"
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
		if url, ok := model.FindMarker(pr, me, marker); ok {
			post.Sent, post.Error = &model.PostReceipt{URL: url, At: now, Recovered: true}, ""
		} else {
			tried = true
			url, err := gh.Post(ctx, pr, post, post.Body+"\n\n"+marker)
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
		if err := gh.React(ctx, first.ID, forge.Reactions[post.React]); err != nil {
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
		if err := gh.ResolveThread(ctx, post.ReplyTo); err != nil {
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
