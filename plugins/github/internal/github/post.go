package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
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
		marker := model.PostMarker(w.Name, id)
		if url, ok := model.FindMarker(pr, me, marker); ok {
			post.Sent, post.Error = &model.PostReceipt{URL: url, At: now, Recovered: true}, ""
		} else if post.Event == "APPROVE" && !approveFresh(ctx, gh.FetchPR, w, pr, post) {
			return
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

// approveFresh re-reads the PR in full right before an APPROVE goes out and
// judges the blocker rule on that read: the round's snapshot
// may carry a PR reused from the stored form, which keeps digests, not
// bodies, so a person's "blocking" thread would read as empty. It says
// whether the approval may go out; when not, it sets post.Error (the read
// failed or was partial: retried next round) or post.Refused (the head moved
// since it was authorized, or a blocker stands: model.ApproveBlocked).
func approveFresh(ctx context.Context, fetch func(ctx context.Context, owner, repo string, n int) (*model.PRState, error),
	w *model.Critic, authorized *model.PRState, post *model.Post) bool {
	fresh, err := fetch(ctx, authorized.Owner, authorized.Repo, authorized.Number)
	switch {
	case err != nil:
		post.Error = "fresh read before the approval failed: " + err.Error()
		return false
	case fresh == nil || !fresh.Complete:
		post.Error = "fresh read before the approval was partial: not approved this round"
		return false
	case fresh.HeadSHA != authorized.HeadSHA:
		post.Refused = fmt.Sprintf("the head moved to %s since the approval was authorized at %s: not approved", fresh.HeadSHA, authorized.HeadSHA)
		return false
	}
	if bs := model.ApproveBlockers(fresh, w.AutomatedMarkers); len(bs) > 0 {
		post.Refused = model.ApproveBlocked + strings.Join(bs, "; ")
		return false
	}
	return true
}
