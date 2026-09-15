package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// backoff mirrors prangl's proven policy: five attempts, 30s doubling to a
// 30m cap. Because this runs as a timer rather than a daemon, a retry fires
// on the first round past its deadline, not at the instant it comes due.
const (
	maxRetries  = 5
	baseBackoff = 30 * time.Second
	maxBackoff  = 30 * time.Minute
)

type Poller struct {
	Store  *Store
	GH     *GH
	Fig    *Figaro
	Out    io.Writer
	DryRun bool
	Me     string
}

// Poll runs exactly one reconciliation round. It is written to be safe to
// call at any cadence, from any number of overlapping invocations (systemd
// guarantees only one per instance), and to exit 0 on "nothing to do".
func (p *Poller) Poll(ctx context.Context, name string) error {
	w, err := p.Store.LoadCritic(name)
	if err != nil {
		return err
	}
	if w.Runtime.Archived {
		fmt.Fprintf(p.Out, "%s: archived, nothing to do\n", name)
		return nil
	}
	if w.Runtime.Stopped {
		fmt.Fprintf(p.Out, "%s: stopped (%s)\n", name, w.Runtime.StopReason)
		return nil
	}

	// gh is still a subprocess, so it still needs its absolute path: a
	// systemd user unit has no interactive PATH. figaro no longer does --
	// it is a socket now.
	if w.GHBin != "" {
		p.GH.Bin = w.GHBin
	}
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
		_ = p.Store.SaveCritic(w)
		p.projectSync(ctx, w, nil, nil)
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
		_ = p.Store.SaveCritic(w)
		fmt.Fprintf(p.Out, "%s: no PRs in scope\n", name)
		return nil
	}

	// --- fetch ---
	next := &Snapshot{
		Critic: name, Round: w.Runtime.Round, TakenAt: time.Now().UTC(),
		PRs: map[PRKey]*PRState{}, Complete: true,
	}
	var fetchErrs []string
	for _, ref := range w.PRs {
		st, ferr := p.GH.FetchPR(ctx, ref.Owner, ref.Repo, ref.Number)
		if ferr != nil {
			fetchErrs = append(fetchErrs, fmt.Sprintf("%s: %v", ref.Key(), ferr))
			// One PR we could not read makes the SCAN partial. The differ
			// will refuse to infer deletions; it must not conclude that a
			// PR vanished because a request timed out.
			next.Complete = false
			// Carry the previous state forward so the PR does not blink
			// out of the projection.
			if prev, _ := p.Store.LoadSnapshot(name); prev != nil {
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

	prev, err := p.Store.LoadSnapshot(name)
	if err != nil {
		return fmt.Errorf("load previous snapshot: %w", err)
	}

	if len(fetchErrs) == len(w.PRs) {
		// Total failure: change nothing, do not diff, count the failure.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = fmt.Sprintf("all %d PR fetches failed: %s", len(w.PRs), fetchErrs[0])
		_ = p.Store.SaveCritic(w)
		p.projectSync(ctx, w, prev, nil)
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
		if err := p.Store.SaveSnapshot(name, next); err != nil {
			return err
		}
		return p.Store.SaveCritic(w)
	}

	// --- accumulate the debt BEFORE attempting delivery ---
	if len(delta.Events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = next.TakenAt
		}
		w.Runtime.PendingEvents = mergeEvents(w.Runtime.PendingEvents, delta.Events)
	}

	// The snapshot advances even if delivery fails. The pending-event list
	// is the delivery debt; the snapshot is the observation. Conflating
	// them re-reports every event on every failed round.
	if err := p.Store.SaveSnapshot(name, next); err != nil {
		return err
	}

	p.projectAll(ctx, w, next, delta)

	// --- deliver ---
	if err := p.deliver(ctx, w, next); err != nil {
		_ = p.Store.SaveCritic(w)
		// Re-project so the form shows the failed attempt and its backoff,
		// not the optimistic state we wrote a moment ago.
		p.projectDelta(ctx, w)
		return err
	}

	// Re-project after a successful send: the write above deliberately
	// happened FIRST so a crash mid-send leaves the debt visible, which
	// means the form is now stale by exactly one delivery. A form that says
	// `pending: true` about news already delivered is a small lie that a
	// develop-mode seat will (correctly) file a bug about.
	p.projectDelta(ctx, w)

	// Succession runs on the same heartbeat: the timer already fires every
	// 5m and already knows the seat, so this needs no second daemon.
	p.superviseSuccession(ctx, w)

	if err := p.Store.SaveCritic(w); err != nil {
		return err
	}
	return p.maybeArchive(ctx, w, next)
}

func (p *Poller) deliver(ctx context.Context, w *Critic, snap *Snapshot) error {
	pending := &Delta{Events: w.Runtime.PendingEvents}
	if !pending.WakeWorthy() {
		if len(w.Runtime.PendingEvents) > 0 {
			// Background-only news is recorded on the form and then
			// forgiven: it must never accumulate into a wake.
			w.Runtime.PendingEvents = nil
			w.Runtime.PendingSince = time.Time{}
		}
		fmt.Fprintf(p.Out, "%s: round %d quiet (%d background event(s))\n",
			w.Name, w.Runtime.Round, len(pending.Events))
		return nil
	}

	if !w.Runtime.NextRetryAt.IsZero() && time.Now().Before(w.Runtime.NextRetryAt) {
		fmt.Fprintf(p.Out, "%s: delivery backoff until %s\n", w.Name, w.Runtime.NextRetryAt.Format(time.RFC3339))
		return nil
	}

	target := w.FormID
	if !p.roleHasHolder(ctx, w.FormID) {
		w.Runtime.LastError = "role has no reachable target-aria; cast a figaro into " + w.FormID
		fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.LastError)
		return nil // not an error exit: an empty seat is an operator condition
	}

	brief := buildBrief(w, snap, w.Runtime.PendingEvents, p.Me, w.Runtime.RetryCount)
	if err := p.Fig.Send(ctx, target, brief); err != nil {
		w.Runtime.RetryCount++
		if w.Runtime.RetryCount >= maxRetries {
			w.Runtime.LastError = fmt.Sprintf("delivery failed %d times, giving up on this batch: %v",
				w.Runtime.RetryCount, err)
			w.Runtime.PendingEvents = nil
			w.Runtime.RetryCount = 0
			w.Runtime.NextRetryAt = time.Time{}
			return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
		}
		w.Runtime.NextRetryAt = time.Now().Add(backoffFor(w.Runtime.RetryCount))
		w.Runtime.LastError = fmt.Sprintf("delivery attempt %d failed: %v", w.Runtime.RetryCount, err)
		return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
	}

	fmt.Fprintf(p.Out, "%s: alerted %s — %d event(s), %d wake-worthy\n",
		w.Name, target, len(w.Runtime.PendingEvents), len(pending.Tier1()))
	w.Runtime.PendingEvents = nil
	w.Runtime.PendingSince = time.Time{}
	w.Runtime.RetryCount = 0
	w.Runtime.NextRetryAt = time.Time{}
	w.Runtime.LastDelivered = time.Now().UTC()
	w.Runtime.DeliveredRound = w.Runtime.Round
	return nil
}

func (p *Poller) roleHasHolder(ctx context.Context, formID string) bool {
	form, err := p.Fig.Form(ctx, formID)
	if err != nil {
		return false
	}
	target, _ := form["target-aria"].(string)
	if target == "" {
		return false
	}
	return p.Fig.AriaExists(ctx, target)
}

func backoffFor(attempt int) time.Duration {
	d := baseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// mergeEvents appends new events to an undelivered batch, dropping exact
// repeats so a stuck delivery does not grow without bound.
func mergeEvents(pending, fresh []Event) []Event {
	seen := map[string]bool{}
	key := func(e Event) string {
		return string(e.Kind) + "|" + string(e.PR) + "|" + e.ThreadID + "|" + e.Comment + "|" + e.ReviewID + "|" + e.Detail
	}
	out := make([]Event, 0, len(pending)+len(fresh))
	for _, e := range pending {
		if !seen[key(e)] {
			seen[key(e)] = true
			out = append(out, e)
		}
	}
	for _, e := range fresh {
		if !seen[key(e)] {
			seen[key(e)] = true
			out = append(out, e)
		}
	}
	const cap = 200
	if len(out) > cap {
		out = out[len(out)-cap:]
	}
	return out
}

func hardStop(w *Critic) string {
	if w.Stop.MaxRounds > 0 && w.Runtime.Round > w.Stop.MaxRounds {
		return fmt.Sprintf("max rounds (%d) reached", w.Stop.MaxRounds)
	}
	if w.Stop.Deadline != nil && time.Now().After(*w.Stop.Deadline) {
		return "deadline " + w.Stop.Deadline.UTC().Format(time.RFC3339) + " passed"
	}
	if w.Stop.MaxConsecutiveFailures > 0 && w.Runtime.ConsecutiveFailures >= w.Stop.MaxConsecutiveFailures {
		return fmt.Sprintf("%d consecutive failures (last: %s)", w.Runtime.ConsecutiveFailures, w.Runtime.LastError)
	}
	return ""
}

// maybeArchive retires a critic whose PRs are all finished. The grace period
// exists because the most interesting comment on a PR often arrives just
// after it merges.
func (p *Poller) maybeArchive(ctx context.Context, w *Critic, snap *Snapshot) error {
	if !w.Stop.UntilAllClosed && !w.Runtime.Stopped {
		return nil
	}
	if snap != nil && len(snap.PRs) > 0 {
		allClosed := true
		for _, pr := range snap.PRs {
			if !pr.Closed() {
				allClosed = false
				break
			}
		}
		if !allClosed {
			if w.Runtime.AllClosedSince != nil {
				w.Runtime.AllClosedSince = nil
				_ = p.Store.SaveCritic(w)
			}
			return nil
		}
		if w.Runtime.AllClosedSince == nil {
			now := time.Now().UTC()
			w.Runtime.AllClosedSince = &now
			_ = p.Store.SaveCritic(w)
			fmt.Fprintf(p.Out, "%s: all PRs closed; grace period %s begins\n",
				w.Name, time.Duration(w.Stop.ArchiveGraceSec)*time.Second)
			return nil
		}
		grace := time.Duration(w.Stop.ArchiveGraceSec) * time.Second
		if time.Since(*w.Runtime.AllClosedSince) < grace {
			return nil
		}
		if w.Runtime.StopReason == "" {
			w.Runtime.StopReason = "all PRs closed or merged"
		}
	} else if !w.Runtime.Stopped {
		return nil
	}

	w.Runtime.Archived = true
	w.Runtime.ArchivedAt = time.Now().UTC()
	w.Runtime.Stopped = true

	// Tell the seat it is over, once, before the form goes away.
	if p.roleHasHolder(ctx, w.FormID) {
		msg := fmt.Sprintf("ebac %s is closing: %s.\nFinal state: %s\nNo further heartbeats will arrive for this critic. "+
			"If you were holding findings for it, report them to the operator now.",
			w.Name, w.Runtime.StopReason, summariseFinal(snap))
		if err := p.Fig.Send(ctx, w.FormID, msg); err != nil {
			fmt.Fprintf(p.Out, "%s: could not deliver closing notice: %v\n", w.Name, err)
		}
	}
	_ = p.projectSync(ctx, w, snap, nil)

	dir, err := p.Store.Archive(w, snap)
	if err != nil {
		return fmt.Errorf("archive %s: %w", w.Name, err)
	}
	fmt.Fprintf(p.Out, "%s: ARCHIVED to %s (%s)\n", w.Name, dir, w.Runtime.StopReason)

	// The systemd timer is disabled but the form is left standing: the
	// operator (or the seat) may still want to read it. `ebac gc` reaps
	// forms whose critic is archived.
	if err := disableTimer(w.Name); err != nil {
		fmt.Fprintf(p.Out, "%s: could not disable timer: %v\n", w.Name, err)
	}
	return nil
}

func summariseFinal(snap *Snapshot) string {
	if snap == nil || len(snap.PRs) == 0 {
		return "no PR state recorded"
	}
	var parts []string
	for _, k := range snap.SortedPRKeys() {
		pr := snap.PRs[k]
		parts = append(parts, fmt.Sprintf("%s %s", k, pr.State))
	}
	sort.Strings(parts)
	return joinMax(parts, 6)
}

func joinMax(parts []string, n int) string {
	if len(parts) <= n {
		return fmt.Sprint(parts)
	}
	return fmt.Sprintf("%v (+%d more)", parts[:n], len(parts)-n)
}

// projectAll writes the whole projection. Every write is a top-level key
// holding a complete value — see Figaro.SetJSON for why that is not a style
// preference.
func (p *Poller) projectAll(ctx context.Context, w *Critic, snap *Snapshot, delta *Delta) {
	f := p.Fig
	set := func(k string, v any) {
		if err := f.SetJSON(ctx, w.FormID, k, v); err != nil {
			fmt.Fprintf(os.Stderr, "ebac: form write %s.%s: %v\n", w.FormID, k, err)
		}
	}
	set("kind", "ebac.critic")
	set("critic", projectCritic(w))
	set("harness", harnessContract(w))
	set("prs", projectPRs(snap, p.Me))
	set("delta", projectDelta(w.Runtime.Round, w.Runtime.PendingEvents,
		len(w.Runtime.PendingEvents) > 0, w.Runtime.PendingSince, w.Runtime.RetryCount))
	set("sync", syncProjection(w, snap, delta))
}

func (p *Poller) projectSync(ctx context.Context, w *Critic, snap *Snapshot, delta *Delta) error {
	return p.Fig.SetJSON(ctx, w.FormID, "sync", syncProjection(w, snap, delta))
}

// projectDelta rewrites just the delta key from current runtime state.
func (p *Poller) projectDelta(ctx context.Context, w *Critic) {
	err := p.Fig.SetJSON(ctx, w.FormID, "delta", projectDelta(w.Runtime.Round,
		w.Runtime.PendingEvents, len(w.Runtime.PendingEvents) > 0,
		w.Runtime.PendingSince, w.Runtime.RetryCount))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ebac: form write %s.delta: %v\n", w.FormID, err)
	}
}

func syncProjection(w *Critic, snap *Snapshot, delta *Delta) SyncProjection {
	s := SyncProjection{
		Round: w.Runtime.Round, OK: w.Runtime.LastError == "",
		LastPolled: w.Runtime.LastPolled.UTC().Format(time.RFC3339),
		Error:      w.Runtime.LastError, ConsecutiveFailures: w.Runtime.ConsecutiveFailures,
		ScanComplete: snap == nil || snap.Complete,
		Stopped:      w.Runtime.Stopped, StopReason: w.Runtime.StopReason,
	}
	if !w.Runtime.LastSuccess.IsZero() {
		s.LastSuccess = w.Runtime.LastSuccess.UTC().Format(time.RFC3339)
	}
	if delta != nil {
		s.SuppressedDeletions = len(delta.SuppressedDeletions)
	}
	return s
}
