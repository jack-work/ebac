package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
	"github.com/gluck/ebac/internal/tomb"
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
	GH     *forge.GH
	Tomb   *tomb.Tomb
	Fig    *Figaro
	Out    io.Writer
	DryRun bool
	Me     string
}

// Poll runs exactly one reconciliation round. It is written to be safe to
// call at any cadence, from any number of overlapping invocations (systemd
// guarantees only one per instance), and to exit 0 on "nothing to do".
func (p *Poller) Poll(ctx context.Context, name string) error {
	w, err := p.Store.LoadCritic(ctx, name)
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
	src, ok := sources[w.SourceName()]
	if !ok {
		return fmt.Errorf("%s: unknown source %q", name, w.SourceName())
	}
	return src.Round(ctx, p, w)
}

func (p *Poller) deliver(ctx context.Context, w *model.Critic, snap *model.Snapshot) error {
	pending := &model.Delta{Events: w.Runtime.PendingEvents}
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
func mergeEvents(pending, fresh []model.Event) []model.Event {
	seen := map[string]bool{}
	key := func(e model.Event) string {
		return string(e.Kind) + "|" + string(e.PR) + "|" + e.ThreadID + "|" + e.Comment + "|" + e.ReviewID + "|" + e.Detail
	}
	out := make([]model.Event, 0, len(pending)+len(fresh))
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

func hardStop(w *model.Critic) string {
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
func (p *Poller) maybeArchive(ctx context.Context, w *model.Critic, snap *model.Snapshot) error {
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
				_ = p.Store.SaveCritic(ctx, w)
			}
			return nil
		}
		if w.Runtime.AllClosedSince == nil {
			now := time.Now().UTC()
			w.Runtime.AllClosedSince = &now
			_ = p.Store.SaveCritic(ctx, w)
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
	dir, err := p.Store.Archive(ctx, w, snap)
	if err != nil {
		return fmt.Errorf("archive %s: %w", w.Name, err)
	}
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return fmt.Errorf("archive %s: %w", w.Name, err)
	}
	fmt.Fprintf(p.Out, "%s: ARCHIVED to %s (%s)\n", w.Name, dir, w.Runtime.StopReason)

	// The timer is disabled and the form left standing for whoever still
	// wants to read it; nothing renews its lease now, so it expires.
	if err := systemd.DisableTimer(w.Name); err != nil {
		fmt.Fprintf(p.Out, "%s: could not disable timer: %v\n", w.Name, err)
	}
	return nil
}

func summariseFinal(snap *model.Snapshot) string {
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
