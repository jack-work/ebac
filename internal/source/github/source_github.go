package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Plugin translates GitHub pull requests: threads, reviews and PR comments
// become the critic's snapshot, its events and its inbox, and authorized
// posts go to the forge. It is the github-pr half of ebac-gh.
type Plugin struct {
	// GH, when set, is used as is (tests); otherwise each request builds a
	// client from the core's --gh/--host and the critic's host.
	GH *forge.GH
}

func (pl Plugin) client(req plugin.Request) *forge.GH {
	gh := pl.GH
	if gh == nil {
		gh = forge.NewGH(req.GH, req.Host)
	}
	if req.Critic.Host != "" {
		gh.Host = req.Critic.Host
	}
	return gh
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	gh := pl.client(req)
	me := req.Me
	rep := &plugin.Reply{}
	if me == "" {
		if m, err := gh.Whoami(ctx); err == nil {
			me, rep.Me = m, m
		}
	}
	switch req.Op {
	case plugin.OpRound:
		round(ctx, gh, req, me, rep)
	case plugin.OpPost:
		rep.Posts = map[string]*model.Post{}
		for id, post := range req.Posts {
			sendPost(ctx, gh, &req.Critic, req.Snapshot, me, id, post)
			rep.Posts[id] = post
		}
	case plugin.OpBrief:
		rep.Brief = brief.BuildBrief(&req.Critic, req.Snapshot, plugin.Unwire(req.Pending), me, req.Retry)
	default:
		return nil, fmt.Errorf("op %q not supported", req.Op)
	}
	return rep, nil
}

// A PR the forge says does not exist (deleted, or its repository renamed or
// gone) on prGoneAfter consecutive rounds is gone. Closed and merged PRs are
// not gone: they take the grace path to archival.
const prGoneAfter = 3

var prGone = regexp.MustCompile(`Could not resolve to a (PullRequest|Repository)|\bnot found\b|\(HTTP 404\)`)

// retireGone marks each gone PR, stops polling it and wakes the seat once; a
// critic with no PR left stops.
func retireGone(rep *plugin.Reply, w *model.Critic, gone []model.PRRef, errs map[model.PRKey]string) {
	now := time.Now().UTC().Format(time.RFC3339)
	keep := w.PRs[:0]
	isGone := map[model.PRKey]bool{}
	for _, r := range gone {
		isGone[r.Key()] = true
	}
	for _, r := range w.PRs {
		if !isGone[r.Key()] {
			keep = append(keep, r)
		}
	}
	w.PRs = keep
	var names []string
	for _, r := range gone {
		k := r.Key()
		names = append(names, string(k))
		delete(w.Runtime.GoneStreak, string(k))
		action := "this PR is no longer polled. If it moved (repo renamed or PR recreated), make a NEW critic for it: never repoint this one."
		if len(w.PRs) == 0 {
			action = "polling stopped. Delete this form and the critic (ebac rm --critic " + w.Name + " --purge); a moved PR needs a NEW critic."
		}
		rep.Gone = append(rep.Gone, plugin.Gone{Key: "pr:" + form.PrFormKey(k), Resource: "pull request " + string(k),
			Since: now, Error: errors.New(errs[k]).Error(), Action: action})
	}
	if len(w.PRs) == 0 {
		w.Runtime.Stopped = true
		w.Runtime.StopReason = "every pull request is gone: " + strings.Join(names, ", ")
		rep.DisableTimer = true
	}
	rep.Wake = append(rep.Wake, fmt.Sprintf("ebac %s: %s gone (not found on %d consecutive rounds). The form's key gone says what to do.",
		w.Name, strings.Join(names, ", "), prGoneAfter))
}

func round(ctx context.Context, gh *forge.GH, req plugin.Request, me string, rep *plugin.Reply) {
	w := &req.Critic
	rep.Critic = w
	name := w.Name

	// --- hard stops, checked before we spend an API call ---
	if reason := w.HardStop(); reason != "" {
		w.Runtime.Stopped, w.Runtime.StopReason = true, reason
		rep.Logf("%s: stopping — %s", name, reason)
		rep.Done, rep.Archive = true, true
		return
	}

	// --- discovery: keep the PR set live ---
	if w.DiscoverRepo != "" {
		refs, derr := gh.ListPRs(ctx, w.DiscoverRepo, w.DiscoverAuthor, "open", 100)
		if derr != nil {
			// Discovery failure must not look like "no PRs match".
			rep.Logf("%s: discovery failed (retained %d known PRs): %v", name, len(w.PRs), derr)
		} else {
			added := 0
			for _, r := range refs {
				if w.AddPR(r) {
					added++
				}
			}
			if added > 0 {
				rep.Logf("%s: discovery added %d PR(s)", name, added)
			}
		}
	}

	if len(w.PRs) == 0 {
		w.Runtime.LastError = "critic has no pull requests"
		rep.Logf("%s: no PRs in scope", name)
		rep.Done = true
		return
	}

	// --- fetch ---
	prev := req.Snapshot
	next := &model.Snapshot{
		Critic: name, Round: w.Runtime.Round, TakenAt: time.Now().UTC(),
		PRs: map[model.PRKey]*model.PRState{}, Complete: true,
	}
	var fetchErrs []string
	var gone []model.PRRef
	for _, ref := range w.PRs {
		st, ferr := gh.FetchPR(ctx, ref.Owner, ref.Repo, ref.Number)
		if ferr != nil && prGone.MatchString(ferr.Error()) {
			if w.Runtime.GoneStreak == nil {
				w.Runtime.GoneStreak = map[string]int{}
			}
			w.Runtime.GoneStreak[string(ref.Key())]++
			if w.Runtime.GoneStreak[string(ref.Key())] >= prGoneAfter {
				gone = append(gone, ref)
			}
		} else {
			delete(w.Runtime.GoneStreak, string(ref.Key()))
		}
		if ferr != nil {
			fetchErrs = append(fetchErrs, fmt.Sprintf("%s: %v", ref.Key(), ferr))
			// One PR we could not read makes the SCAN partial. The differ
			// will refuse to infer deletions; it must not conclude that a
			// PR vanished because a request timed out.
			next.Complete = false
			// Carry the previous state forward so the PR does not blink
			// out of the projection.
			if prev != nil {
				if old, ok := prev.PRs[ref.Key()]; ok {
					old.Complete = false
					next.PRs[ref.Key()] = old
				}
			}
			continue
		}
		if !st.Complete {
			next.Complete = false
		}
		next.PRs[st.Key] = st
	}
	if len(gone) > 0 {
		errs := map[model.PRKey]string{}
		for _, fe := range fetchErrs {
			if k, msg, ok := strings.Cut(fe, ": "); ok {
				errs[model.PRKey(k)] = msg
			}
		}
		retireGone(rep, w, gone, errs)
		if w.Runtime.Stopped {
			rep.Done = true
			return
		}
	}

	if len(fetchErrs) == len(w.PRs) {
		// Total failure: change nothing, do not diff, count the failure.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = fmt.Sprintf("all %d PR fetches failed: %s", len(w.PRs), fetchErrs[0])
		rep.Error = w.Runtime.LastError
		return
	}

	if len(fetchErrs) > 0 {
		w.Runtime.LastError = fmt.Sprintf("%d/%d PR fetches failed: %s", len(fetchErrs), len(w.PRs), fetchErrs[0])
	} else {
		w.Runtime.LastError = ""
		w.Runtime.ConsecutiveFailures = 0
		w.Runtime.LastSuccess = next.TakenAt
	}

	delta := Diff(prev, next, me)
	if w.Mode == model.ModeReviewer {
		AsReviewer(delta, me)
	}
	if req.DryRun && len(delta.SuppressedDeletions) > 0 {
		rep.Logf("%s: deletions suppressed for %v (partial scan)", name, delta.SuppressedDeletions)
	}

	// The source's tiers are a suggestion; the core's trigger rules decide.
	now := time.Now()
	rep.Inbox = map[string]form.InboxItem{}
	rep.Events = plugin.Wire(delta.Events)
	for i, e := range delta.Events {
		for k, it := range form.InboxItems(next, []model.Event{e}, me, now) {
			rep.Inbox[k] = it
			rep.Events[i].Inbox = append(rep.Events[i].Inbox, k)
		}
	}
	rep.Snapshot = next
	rep.Outbox, rep.Succession, rep.Archive = true, true, true
}

func postMarker(critic, id string) string {
	return fmt.Sprintf("<!-- ebac:post:%s:%s -->", critic, id)
}
