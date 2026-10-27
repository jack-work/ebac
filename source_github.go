package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// githubPRs translates GitHub pull requests: threads, reviews and PR comments
// become the critic's prs state, its inbox and its outbox.
type githubPRs struct{}

func (githubPRs) Name() string { return "github-pr" }

func (githubPRs) Round(ctx context.Context, p *Poller, w *model.Critic) error {
	return pollGitHub(ctx, p, w)
}

// A PR the forge says does not exist (deleted, or its repository renamed or
// gone) on prGoneAfter consecutive rounds is gone. Closed and merged PRs are
// not gone: they take the grace path to archival.
const prGoneAfter = 3

var prGone = regexp.MustCompile(`Could not resolve to a (PullRequest|Repository)|\bnot found\b|\(HTTP 404\)`)

// retireGone marks each gone PR on the form, stops polling it and wakes the
// seat once; a critic with no PR left stops.
func retireGone(ctx context.Context, p *Poller, w *model.Critic, gone []model.PRRef, errs map[model.PRKey]string) error {
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
		if err := p.markGone(ctx, w, "pr:"+form.PrFormKey(k), "pull request "+string(k), now, errors.New(errs[k]), action); err != nil {
			return err
		}
	}
	if len(w.PRs) == 0 {
		w.Runtime.Stopped = true
		w.Runtime.StopReason = "every pull request is gone: " + strings.Join(names, ", ")
		_ = systemd.DisableTimer(w.Name)
	}
	p.wakeOnce(ctx, w, fmt.Sprintf("ebac %s: %s gone (not found on %d consecutive rounds). The form's key gone says what to do.",
		w.Name, strings.Join(names, ", "), prGoneAfter))
	return p.Store.SaveCritic(ctx, w)
}

func pollGitHub(ctx context.Context, p *Poller, w *model.Critic) error {
	name := w.Name
	if w.Host != "" {
		p.GH.Host = w.Host
	}

	w.Runtime.Round++
	w.Runtime.LastPolled = time.Now().UTC()

	if p.Me == "" {
		if me, err := p.GH.Whoami(ctx); err == nil {
			p.Me = me
		}
	}

	// --- hard stops, checked before we spend an API call ---
	if reason := hardStop(w); reason != "" {
		w.Runtime.Stopped, w.Runtime.StopReason = true, reason
		_ = p.Store.SaveCritic(ctx, w)
		fmt.Fprintf(p.Out, "%s: stopping — %s\n", name, reason)
		return p.maybeArchive(ctx, w, nil)
	}

	// --- discovery: keep the PR set live ---
	if w.DiscoverRepo != "" {
		refs, derr := p.GH.ListPRs(ctx, w.DiscoverRepo, w.DiscoverAuthor, "open", 100)
		if derr != nil {
			// Discovery failure must not look like "no PRs match".
			fmt.Fprintf(p.Out, "%s: discovery failed (retained %d known PRs): %v\n", name, len(w.PRs), derr)
		} else {
			added := 0
			for _, r := range refs {
				if w.AddPR(r) {
					added++
				}
			}
			if added > 0 {
				fmt.Fprintf(p.Out, "%s: discovery added %d PR(s)\n", name, added)
			}
		}
	}

	if len(w.PRs) == 0 {
		w.Runtime.LastError = "critic has no pull requests"
		_ = p.Store.SaveCritic(ctx, w)
		fmt.Fprintf(p.Out, "%s: no PRs in scope\n", name)
		return nil
	}

	// --- fetch ---
	next := &model.Snapshot{
		Critic: name, Round: w.Runtime.Round, TakenAt: time.Now().UTC(),
		PRs: map[model.PRKey]*model.PRState{}, Complete: true,
	}
	var fetchErrs []string
	var gone []model.PRRef
	for _, ref := range w.PRs {
		st, ferr := p.GH.FetchPR(ctx, ref.Owner, ref.Repo, ref.Number)
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
			if prev, _ := p.Store.LoadSnapshot(ctx, name); prev != nil {
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
		if err := retireGone(ctx, p, w, gone, errs); err != nil {
			return err
		}
		if w.Runtime.Stopped {
			return nil
		}
	}

	prev, err := p.Store.LoadSnapshot(ctx, name)
	if err != nil {
		return fmt.Errorf("load previous snapshot: %w", err)
	}

	if len(fetchErrs) == len(w.PRs) {
		// Total failure: change nothing, do not diff, count the failure.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = fmt.Sprintf("all %d PR fetches failed: %s", len(w.PRs), fetchErrs[0])
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %s", name, w.Runtime.LastError)
	}

	if len(fetchErrs) > 0 {
		w.Runtime.LastError = fmt.Sprintf("%d/%d PR fetches failed: %s", len(fetchErrs), len(w.PRs), fetchErrs[0])
	} else {
		w.Runtime.LastError = ""
		w.Runtime.ConsecutiveFailures = 0
		w.Runtime.LastSuccess = next.TakenAt
	}

	delta := Diff(prev, next, p.Me)
	if w.Mode == model.ModeReviewer {
		asReviewer(delta, p.Me)
	}

	if p.DryRun {
		fmt.Fprintf(p.Out, "%s: DRY RUN round %d — %s (%d events, %d wake-worthy)\n",
			name, w.Runtime.Round, delta.Summary(), len(delta.Events), len(delta.Tier1()))
		for _, e := range delta.Events {
			fmt.Fprintf(p.Out, "  [t%d] %s\n", e.Tier, e.Line())
		}
		if len(delta.SuppressedDeletions) > 0 {
			fmt.Fprintf(p.Out, "  NOTE: deletions suppressed for %v (partial scan)\n", delta.SuppressedDeletions)
		}
		// A dry run still persists observed state — that is what makes the
		// FIRST armed round quiet instead of an avalanche.
		return p.Store.Save(ctx, w, next, nil)
	}

	// --- outbox: what the seat wrote onto the form goes to the forge ---
	acks := p.flushOutbox(ctx, w, next)

	// --- accumulate the debt BEFORE attempting delivery ---
	if len(delta.Events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = next.TakenAt
		}
		w.Runtime.PendingEvents = mergeEvents(w.Runtime.PendingEvents, delta.Events)
	}

	// One patch: the observed PRs, the debt and the outbox receipts. It lands
	// before delivery, so a crash mid-send leaves the debt on the form and
	// the seat's wake finds the round's change already in its study diff.
	if err := p.Store.SaveRound(ctx, w, next, acks, form.InboxItems(next, delta.Events, p.Me, time.Now())); err != nil {
		return err
	}
	figaro.Renew(ctx, p.Fig, w.FormID)

	derr := p.deliver(ctx, w, next)
	if derr == nil {
		// Succession runs on the same heartbeat: the timer already fires
		// every 5m and already knows the seat.
		p.superviseSuccession(ctx, w)
	}
	// Runtime only, under system.ebac: invisible to the seat.
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	if derr != nil {
		return derr
	}
	return p.maybeArchive(ctx, w, next)
}
