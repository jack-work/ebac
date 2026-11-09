package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/systemd"
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
	Store *form.Store
	Fig   *figaro.Figaro
	// Plugins runs the source plugins: a plugin.Runner in production, a
	// plugin.InProc in tests.
	Plugins plugin.Caller
	Out     io.Writer
	DryRun  bool
	Me      string
	// GH and Host are the --gh and --host flags, passed through to plugins.
	GH   string
	Host string
}

func (p *Poller) call(ctx context.Context, w *model.Critic, req plugin.Request) (*plugin.Reply, error) {
	name, err := plugin.For(w.SourceName())
	if err != nil {
		return nil, err
	}
	req.Critic = *w
	req.Me, req.GH, req.Host, req.DryRun = p.Me, p.GH, p.Host, p.DryRun
	return p.Plugins.Call(ctx, name, req)
}

// Poll runs exactly one reconciliation round. It is written to be safe to
// call at any cadence, from any number of overlapping invocations (systemd
// guarantees only one per instance), and to exit 0 on "nothing to do".
//
// The source plugin does the upstream half of the round (plugin.OpRound);
// everything else here is the core's: trigger rules, pending events, the
// one form patch, the outbox, delivery, succession and archival.
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
	if err := p.migrateQuiet(ctx, w); err != nil {
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	prev, err := p.Store.LoadSnapshot(ctx, name)
	if err != nil {
		return fmt.Errorf("load previous snapshot: %w", err)
	}

	w.Runtime.Round++
	w.Runtime.LastPolled = time.Now().UTC()
	rep, err := p.call(ctx, w, plugin.Request{Op: plugin.OpRound, Snapshot: prev, Seat: p.seat(ctx, w.AlertTarget())})
	if err == nil {
		err = SourceKeysOnly(w.SourceName(), rep)
	}
	if err != nil {
		// The plugin itself failed (crash, timeout, bad reply): a failed
		// round like any upstream failure.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %w", name, err)
	}
	return p.apply(ctx, w, rep)
}

// SourceKeys are, per source, the only role-form keys its plugin may set or
// unset (derived from every SetKey/Unset in the plugins). Every other key on
// the form (target-aria, trigger, succession.*, post, inbox, prs, ...) is the
// core's or the seat's, and a plugin that names one is refused before any of
// its reply is applied.
var SourceKeys = map[string][]string{
	"github-pr":         nil,
	"github-author":     {"authored"},
	"keel-threads":      {"threads", "blind"},
	"teams-chat":        {"blind"},
	model.SourceBandHub: {"queues", "nodes", "live", "dupes", "updated"},
}

// SourceKeysOnly refuses a reply that writes outside its source's keys,
// naming the key and the plugin.
func SourceKeysOnly(source string, rep *plugin.Reply) error {
	if source == "" {
		source = "github-pr"
	}
	name, _ := plugin.For(source)
	allowed := SourceKeys[source]
	check := func(verb, k string) error {
		for _, a := range allowed {
			if a == k {
				return nil
			}
		}
		own := "none"
		if len(allowed) > 0 {
			own = strings.Join(allowed, ", ")
		}
		return fmt.Errorf("plugin %s may not %s form key %q (%s keys: %s): reply refused", name, verb, k, source, own)
	}
	for k := range rep.Set {
		if err := check("set", k); err != nil {
			return err
		}
	}
	for _, k := range rep.Unset {
		if err := check("unset", k); err != nil {
			return err
		}
	}
	return nil
}

// apply lands one round's reply.
func (p *Poller) apply(ctx context.Context, w *model.Critic, rep *plugin.Reply) error {
	for _, l := range rep.Log {
		fmt.Fprintln(p.Out, l)
	}
	if rep.Me != "" {
		p.Me = rep.Me
	}
	plugin.Adopt(w, rep.Critic)
	for _, g := range rep.Gone {
		if err := p.MarkGone(ctx, w, g); err != nil {
			return err
		}
	}
	// One-off wakes go out after the keys they point at are written.
	wake := func() {
		for _, msg := range rep.Wake {
			p.WakeOnce(ctx, w, msg)
		}
	}
	if rep.DisableTimer {
		_ = systemd.DisableTimer(w.Name)
	}

	events, inbox := p.classify(ctx, w, rep)

	if rep.Done || rep.Error != "" {
		if err := p.commitKeys(ctx, w, rep.Set, rep.Unset); err != nil {
			return fmt.Errorf("%s: %w", w.Name, err)
		}
		wake()
		if err := p.Store.SaveCritic(ctx, w); err != nil {
			return err
		}
		if rep.Error != "" {
			return fmt.Errorf("%s: %s", w.Name, rep.Error)
		}
		if rep.Archive {
			return p.MaybeArchive(ctx, w, rep.Snapshot)
		}
		return nil
	}

	if p.DryRun {
		d := &model.Delta{Events: events}
		fmt.Fprintf(p.Out, "%s: DRY RUN round %d — %s (%d events, %d wake-worthy)\n",
			w.Name, w.Runtime.Round, d.Summary(), len(events), len(d.Tier1()))
		for _, e := range events {
			fmt.Fprintf(p.Out, "  [t%d] %s\n", e.Tier, e.Line())
		}
		// A dry run still persists observed state: that is what makes the
		// FIRST armed round quiet instead of an avalanche.
		if err := p.commitKeys(ctx, w, rep.Set, rep.Unset); err != nil {
			return fmt.Errorf("%s: %w", w.Name, err)
		}
		return p.Store.Save(ctx, w, rep.Snapshot, nil)
	}

	// --- outbox: what the seat wrote onto the form goes upstream ---
	var acks form.Receipts
	if rep.Outbox {
		acks = p.FlushOutbox(ctx, w, rep.Snapshot)
	}

	// --- accumulate the debt BEFORE attempting delivery ---
	if len(events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = time.Now().UTC()
		}
		w.Runtime.PendingEvents = MergeEvents(w.Runtime.PendingEvents, events)
	}

	// The observed state, the debt, the receipts and the inbox land before
	// delivery, so a crash mid-send leaves the debt on the form and the
	// seat's wake finds the round's change already in its study diff.
	if err := p.Store.SaveRound(ctx, w, rep.Snapshot, acks, inbox); err != nil {
		return err
	}
	if err := p.commitKeys(ctx, w, rep.Set, rep.Unset); err != nil {
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	wake()
	figaro.Renew(ctx, p.Fig, w.FormID)

	derr := p.Deliver(ctx, w, rep.Snapshot)
	if derr == nil && rep.Succession {
		// Succession runs on the same heartbeat: the timer already fires
		// and already knows the seat.
		p.SuperviseSuccession(ctx, w)
	}
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	if derr != nil {
		return derr
	}
	if rep.Archive {
		return p.MaybeArchive(ctx, w, rep.Snapshot)
	}
	return nil
}

// classify runs the trigger rules over the round's events and drops the
// inbox items of every dropped event (unless a kept event shares them).
func (p *Poller) classify(ctx context.Context, w *model.Critic, rep *plugin.Reply) ([]model.Event, map[string]form.InboxItem) {
	inbox := rep.Inbox
	if inbox == nil {
		inbox = map[string]form.InboxItem{}
	}
	evs := plugin.Unwire(rep.Events)
	keysOf := map[string][]string{}
	for i, e := range rep.Events {
		keysOf[eventKey(evs[i])] = e.Inbox
	}
	kept, dropped := p.Classify(ctx, w, evs)
	keep := map[string]bool{}
	for _, e := range kept {
		for _, k := range keysOf[eventKey(e)] {
			keep[k] = true
		}
	}
	for _, e := range dropped {
		for _, k := range keysOf[eventKey(e)] {
			if !keep[k] {
				delete(inbox, k)
			}
		}
	}
	return kept, inbox
}

func eventKey(e model.Event) string {
	return string(e.Kind) + "|" + string(e.PR) + "|" + e.ThreadID + "|" + e.Comment + "|" + e.ReviewID
}

// commitKeys writes the source's own form keys, skipping any whose value is
// unchanged so a quiet round does not move the form version.
func (p *Poller) commitKeys(ctx context.Context, w *model.Critic, set map[string]json.RawMessage, unset []string) error {
	if len(set) == 0 && len(unset) == 0 {
		return nil
	}
	return p.Store.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		out := map[string]json.RawMessage{}
		for k, v := range set {
			if !jsonEqual(cur[k], v) {
				out[k] = v
			}
		}
		var gone []string
		for _, k := range unset {
			if _, ok := cur[k]; ok {
				gone = append(gone, k)
			}
		}
		return out, gone
	})
}

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	return string(model.MustJSON(x)) == string(model.MustJSON(y))
}

// seat is the aria holding a role form, or "".
func (p *Poller) seat(ctx context.Context, formID string) string {
	f, err := p.Fig.Form(ctx, formID)
	if err != nil {
		return ""
	}
	id, _ := f["target-aria"].(string)
	return id
}

// Deliver wakes the critic's alert target with the pending events, rendered
// by the source's plugin (plugin.OpBrief). Background-only news is forgiven;
// a failed send backs off and gives up after maxRetries.
func (p *Poller) Deliver(ctx context.Context, w *model.Critic, snap *model.Snapshot) error {
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

	target := w.AlertTarget()
	if !p.RoleHasHolder(ctx, target) {
		w.Runtime.LastError = "role has no reachable target-aria; cast a figaro into " + target
		fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.LastError)
		return nil // not an error exit: an empty seat is an operator condition
	}

	msg, err := p.brief(ctx, w, snap)
	if err == nil {
		err = p.Fig.Send(ctx, target, msg)
	}
	if err != nil {
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

func (p *Poller) brief(ctx context.Context, w *model.Critic, snap *model.Snapshot) (string, error) {
	rep, err := p.call(ctx, w, plugin.Request{Op: plugin.OpBrief, Snapshot: snap,
		Pending: plugin.Wire(w.Runtime.PendingEvents), Retry: w.Runtime.RetryCount})
	if err != nil {
		return "", err
	}
	if rep.Brief == "" {
		return "", fmt.Errorf("plugin rendered an empty brief")
	}
	return rep.Brief, nil
}

func (p *Poller) RoleHasHolder(ctx context.Context, formID string) bool {
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
func MergeEvents(pending, fresh []model.Event) []model.Event {
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

// maybeArchive retires a critic whose PRs are all finished. The grace period
// exists because the most interesting comment on a PR often arrives just
// after it merges.
func (p *Poller) MaybeArchive(ctx context.Context, w *model.Critic, snap *model.Snapshot) error {
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
	if p.RoleHasHolder(ctx, w.FormID) {
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
