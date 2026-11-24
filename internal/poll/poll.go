package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gluck/ebac/internal/pluginrun"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/otel"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
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
	// Plugins runs the source plugins: a pluginrun.Runner in production, a
	// plugin.InProc in tests.
	Plugins plugin.Caller
	Out     io.Writer
	DryRun  bool
	Me      string
	// GH and Host are the --gh and --host flags, passed through to plugins.
	GH   string
	Host string
	// Send and Holder replace delivery's figaro send and seat check in tests
	// (nil: the real ones), so a delivery cadence can be counted without
	// waking an aria.
	Send func(ctx context.Context, target, msg string) error
	// Kill replaces the broker kill of a form in tests (nil: the broker).
	Kill   func(ctx context.Context, id string) error
	Holder func(ctx context.Context, target string) bool
	// BackoffDir holds the per-critic figaro backoff files ("" = no backoff,
	// as in tests); PauseFile overrides the bounce pause path ("" = PauseFile).
	BackoffDir string
	PauseFile  string
	// OnSpend, when set, receives what each plugin call spent (ebacd settles
	// the budget ledger with it; a poll by hand leaves it nil).
	OnSpend func(spent []model.Spend)
	// OTel, when set, receives each round's plugin record (Reply.Telemetry)
	// and exports it over OTLP (ebacd; nil exports nothing).
	OTel *otel.Exporter
	// Roles replaces the role-name lookup for trigger role_in_path in tests
	// (nil: RoleNames, read from the roster critic's form).
	Roles func(ctx context.Context) (map[string]string, error)
	// RoleCheck replaces CheckRole in tests.
	RoleCheck func(ctx context.Context, caller, role string) (form, why string, err error)
	// dir is this round's identity directory (Directory), read at most once.
	dir *identity.Directory
}

func (p *Poller) call(ctx context.Context, w *model.Critic, req plugin.Request) (*plugin.Reply, error) {
	name, err := pluginrun.For(w.SourceName())
	if err != nil {
		return nil, err
	}
	req.Critic = *w
	req.Me, req.GH, req.Host, req.DryRun = p.Me, p.GH, p.Host, p.DryRun
	rep, err := p.Plugins.Call(ctx, name, req)
	if rep != nil && p.OnSpend != nil && len(rep.Spend) > 0 {
		p.OnSpend(rep.Spend)
	}
	if rep != nil {
		p.export(ctx, w, rep)
	}
	return rep, err
}

// Poll runs exactly one reconciliation round. It is written to be safe to
// call at any cadence, from any number of overlapping invocations (systemd
// guarantees only one per instance), and to exit 0 on "nothing to do".
//
// The source plugin does the upstream half of the round (plugin.OpRound);
// everything else here is the core's: trigger rules, pending events, the
// one form patch, the outbox, delivery and archival.
// Poll runs one round for a critic, unless a figaro bounce has paused ebac
// or the critic is backing off from an unreachable daemon (figbackoff.go).
func (p *Poller) Poll(ctx context.Context, name string) error {
	if Paused(p.PauseFile) {
		fmt.Fprintf(p.Out, "%s: figaro bounce in progress (%s): round skipped\n", name, orDefault(p.PauseFile, PauseFile))
		return nil
	}
	now := time.Now()
	if p.BackoffDir != "" {
		if b := readFigBackoff(p.BackoffDir, name); b.Until.After(now) {
			fmt.Fprintf(p.Out, "%s: backing off from figaro until %s (attempt %d): round skipped\n", name, b.Until.UTC().Format(time.RFC3339), b.Attempt)
			return nil
		}
	}
	if p.BackoffDir != "" {
		unlock, held, lerr := lockRound(p.BackoffDir, name)
		if lerr != nil {
			return lerr
		}
		if held {
			fmt.Fprintf(p.Out, "%s: a round is already running (ebacd or a hand poll): skipped\n", name)
			return nil
		}
		defer unlock()
	}
	err := p.pollRound(ctx, name)
	if p.BackoffDir == "" {
		return err
	}
	if figaro.Unavailable(err) {
		b, werr := noteFigFailure(p.BackoffDir, name, now, err, figRand)
		if werr != nil {
			return fmt.Errorf("%w (and the backoff was not saved: %v)", err, werr)
		}
		return fmt.Errorf("%s", fmtBackoff(name, b))
	}
	clearFigBackoff(p.BackoffDir, name)
	return err
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (p *Poller) pollRound(ctx context.Context, name string) error {
	p.dir = nil // identity is read fresh each round, at most once
	w, err := p.Store.LoadCritic(ctx, name)
	if err != nil {
		if figaro.Unavailable(err) {
			return err // the daemon is not answering: surfacing would only wait on it again
		}
		if berr := p.surfaceBroken(ctx, name); berr != nil {
			return berr
		}
		return err
	}
	if err := refuseNoAuthority(w); err != nil {
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
	req := plugin.Request{Op: plugin.OpRound, Snapshot: prev, Seat: p.seat(ctx, w.AlertTarget()), Keys: p.withReviewCheck(ctx, w)}
	if CommitSources[w.SourceName()] {
		req.CommitToken = newCommitToken()
	}
	if w.SourceName() == model.SourceRoster {
		// The roster's whole input is the aria store, read here with the
		// core's own grant: the plugin has none.
		if req.Arias, err = p.Fig.ListArias(ctx); err != nil {
			err = fmt.Errorf("list arias: %w", err)
		}
	}
	if err == nil && (w.SourceName() == model.SourceKeelBranches || w.SourceName() == model.SourceGHCache) {
		// Keel branches cite their child forms and read each one's
		// holder-owned keys (code_ready, ready) and holder; the GitHub cache
		// reads each entity form's gh and sync, where seats edit (DESIGN 2).
		req.Children, req.Owned = w.Runtime.Children, p.readOwned(ctx, w)
	}
	if err == nil && w.SourceName() == model.SourceScout {
		// The scout learns which of its entries are on its form (an entry it no longer tracks is removed).
		var f *form.CriticForm
		if f, err = p.Store.ReadForm(ctx, w.FormID); err == nil {
			for k := range f.Keys {
				for _, pat := range SourceKeys[model.SourceScout] {
					if strings.HasSuffix(pat, "*") && keyAllowed(pat, k) {
						req.OwnKeys = append(req.OwnKeys, k)
					}
				}
			}
			sort.Strings(req.OwnKeys)
		}
	}
	if err == nil && w.SourceName() == model.SourceApprove {
		// approve-requests decides the requests on its inputs form.
		req.Inputs, err = p.readInputs(ctx, w)
	}
	if err == nil && w.SourceName() == model.SourceScout {
		// The scout reads its owners' inputs form (merge rule, ledger acks)
		// and the board's owner marks, read-only, through the core.
		req.Inputs, err = p.readInputs(ctx, w)
		if err == nil {
			req.Board, err = p.readBoard(ctx, w)
		}
	}
	var rep *plugin.Reply
	if err == nil {
		rep, err = p.call(ctx, w, req)
	}
	if err == nil {
		err = SourceKeysOnly(w.SourceName(), rep)
	}
	if err == nil {
		err = ValidateForms(w, rep)
	}
	if err == nil {
		err = ValidatePings(w, rep)
	}
	if err == nil {
		err = ValidateCommit(w.SourceName(), rep, plugin.OpRound)
	}
	if err != nil {
		// The plugin itself failed (crash, timeout, bad reply): a failed
		// round like any upstream failure.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %w", name, err)
	}
	return twoPhase(req, rep, p.DryRun, phases{
		apply: func(r *plugin.Reply) (bool, error) { return p.apply(ctx, w, r) },
		call:  func(r plugin.Request) (*plugin.Reply, error) { return p.call(ctx, w, r) },
		check: func(r *plugin.Reply) error { return checkReply(w, r, plugin.OpCommit) },
	})
}

// SourceKeys are, per source, the only role-form keys its plugin may set or
// unset (derived from every SetKey/Unset in the plugins). Every other key on
// the form (target-aria, trigger, succession.*, post, inbox, prs, ...) is the
// core's or the seat's, and a plugin that names one is refused before any of
// its reply is applied.
var SourceKeys = map[string][]string{
	"github-pr":         nil,
	"github-author":     {"authored", model.AuthorFilterErr},
	"keel-threads":      {"threads", "blind", model.KeelScopeErr, "review_state", "review_error"},
	"teams-chat":        {"blind", "inbox_fetch"},
	model.SourceBandHub: {"queues", "nodes", "node_load", "dupes", "updated", "live"}, // live: unset only (dropped key)
	"github-releases":   {"releases"},
	// role:<name> is one key per role ("*" is a prefix pattern: one more
	// segment, no dots, so it cannot reach a nested key).
	model.SourceRoster: {"role:*", "forks", "updated"}, // updated: unset only (dropped key)
	// One key per PR and per issue, on the scout's own role form.
	model.SourceScout: {"pr:*", "issue:*", "claim:*", "warning", "quiet_pings"},
	// approve-requests writes only results on its inputs form (Reply.Forms).
	model.SourceApprove: nil,
	// org-approve writes only asks on its board (Reply.Forms).
	model.SourceOrgApprove: nil,
	// Keel branches write their child forms (Reply.Forms) and, on their own
	// role form, the registry (branch -> child form) and the branches they
	// could not follow.
	model.SourceKeelBranches: {"branches", "overflow"},
	// The GitHub cache writes its index (one object: figaro#43) on its own
	// role form; config is the holder's, read only.
	model.SourceGHCache: {"track"},
}

func keyAllowed(pattern, k string) bool {
	if pre, ok := strings.CutSuffix(pattern, "*"); ok {
		rest, ok := strings.CutPrefix(k, pre)
		return ok && rest != "" && !strings.ContainsAny(rest, ".*")
	}
	return pattern == k
}

// SourceKeysOnly refuses a reply that writes outside its source's keys,
// naming the key and the plugin.
func SourceKeysOnly(source string, rep *plugin.Reply) error {
	if source == "" {
		source = "github-pr"
	}
	name, _ := pluginrun.For(source)
	allowed := SourceKeys[source]
	check := func(verb, k string) error {
		for _, a := range allowed {
			if keyAllowed(a, k) {
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

// export sends the round's record over OTLP; a failure is said, never fails
// the round.
func (p *Poller) export(ctx context.Context, w *model.Critic, rep *plugin.Reply) {
	if p.OTel == nil || len(rep.Telemetry) == 0 {
		return
	}
	if len(rep.Telemetry) > MaxTelemetry {
		fmt.Fprintf(p.Out, "%s: telemetry record of %d bytes over %d: not exported\n", w.Name, len(rep.Telemetry), MaxTelemetry)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.OTel.Export(ctx, otel.Round{Critic: w.Name, Source: w.SourceName(), Start: w.Runtime.LastPolled, End: time.Now().UTC(),
		Record: rep.Telemetry, Error: rep.Error}); err != nil {
		fmt.Fprintf(p.Out, "%s: telemetry export: %v\n", w.Name, err)
	}
}

// MaxTelemetry caps one round's telemetry record.
const MaxTelemetry = 64 * 1024

// apply lands one round's reply. saved: the critic (its runtime: a
// two-phase source's prepared records) reached the store, the durable write
// an OpCommit may follow (commit.go).
func (p *Poller) apply(ctx context.Context, w *model.Critic, rep *plugin.Reply) (saved bool, err error) {
	for _, l := range rep.Log {
		fmt.Fprintln(p.Out, l)
	}
	if rep.Me != "" {
		p.Me = rep.Me
	}
	plugin.Adopt(w, rep.Critic)
	for _, g := range rep.Gone {
		if err := p.MarkGone(ctx, w, g); err != nil {
			return false, err
		}
	}
	// One-off wakes go out after the keys they point at are written.
	wake := func() {
		for _, msg := range rep.Wake {
			p.WakeOnce(ctx, w, msg)
		}
	}
	events, inbox := p.classify(ctx, w, rep)

	if rep.Done || rep.Error != "" {
		if err := p.commitKeys(ctx, w, rep.Set, rep.Unset); err != nil {
			return false, fmt.Errorf("%s: %w", w.Name, err)
		}
		ferr := p.landForms(ctx, w, rep)
		wake()
		if err := p.Store.SaveCritic(ctx, w); err != nil {
			return false, err
		}
		if ferr != nil {
			return true, ferr
		}
		if rep.Error != "" {
			return true, fmt.Errorf("%s: %s", w.Name, rep.Error)
		}
		if rep.Archive {
			return true, p.MaybeArchive(ctx, w, rep.Snapshot)
		}
		return true, nil
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
			return false, fmt.Errorf("%s: %w", w.Name, err)
		}
		ferr := p.landForms(ctx, w, rep)
		if err := p.Store.Save(ctx, w, rep.Snapshot, nil); err != nil {
			return false, err
		}
		return true, ferr
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
		return false, err
	}
	if err := p.commitKeys(ctx, w, rep.Set, rep.Unset); err != nil {
		return false, fmt.Errorf("%s: %w", w.Name, err)
	}
	ferr := p.landForms(ctx, w, rep)
	wake()
	figaro.Renew(ctx, p.Fig, w.FormID)

	derr := p.Deliver(ctx, w, rep.Snapshot)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return false, err
	}
	if ferr != nil {
		return true, ferr
	}
	if derr != nil {
		return true, derr
	}
	if rep.Archive {
		return true, p.MaybeArchive(ctx, w, rep.Snapshot)
	}
	return true, nil
}

// classify runs the trigger rules over the round's events and drops the
// inbox items of every dropped event (unless a kept event shares them).
func (p *Poller) classify(ctx context.Context, w *model.Critic, rep *plugin.Reply) ([]model.Event, map[string]model.InboxItem) {
	inbox := rep.Inbox
	if inbox == nil {
		inbox = map[string]model.InboxItem{}
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

	if PingHeld(w, time.Now()) {
		fmt.Fprintf(p.Out, "%s: collect: %d wake-worthy held for the ping due %s\n", w.Name, len(pending.Tier1()),
			w.Runtime.LastPing.Add(w.PingEvery()).Format(time.RFC3339))
		return nil
	}

	if !w.Runtime.NextRetryAt.IsZero() && time.Now().Before(w.Runtime.NextRetryAt) {
		fmt.Fprintf(p.Out, "%s: delivery backoff until %s\n", w.Name, w.Runtime.NextRetryAt.Format(time.RFC3339))
		return nil
	}

	holder := p.RoleHasHolder
	if p.Holder != nil {
		holder = p.Holder
	}
	send := p.Fig.Send
	if p.Send != nil {
		send = p.Send
	}

	// One wake per route: a trigger rule's "to" sends its events to that
	// role; the rest go to the critic's alert target. A route with nothing
	// wake-worthy is forgiven like a quiet round; a route whose role has no
	// holder or whose send fails keeps its events for the next round, and
	// the routes delivered are not sent again.
	all := w.Runtime.PendingEvents
	var keep []model.Event
	var errs []string
	delivered := 0
	for _, g := range routeGroups(all, w.AlertTarget()) {
		if !(&model.Delta{Events: g.events}).WakeWorthy() {
			continue
		}
		if !holder(ctx, g.target) {
			w.Runtime.LastError = "role has no reachable target-aria; cast a figaro into " + g.target
			fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.LastError)
			keep = append(keep, g.events...)
			continue
		}
		w.Runtime.PendingEvents = g.events // brief and ping line render this route's events only
		var msg string
		var err error
		if w.PingEvery() > 0 {
			msg = PingLine(w)
		} else {
			msg, err = p.brief(ctx, w, snap)
		}
		if err == nil {
			err = send(ctx, g.target, msg)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", g.target, err))
			keep = append(keep, g.events...)
			continue
		}
		delivered++
		fmt.Fprintf(p.Out, "%s: alerted %s — %d event(s), %d wake-worthy\n",
			w.Name, g.target, len(g.events), len((&model.Delta{Events: g.events}).Tier1()))
	}
	w.Runtime.PendingEvents = keep
	if len(errs) > 0 {
		err := strings.Join(errs, "; ")
		w.Runtime.RetryCount++
		if w.Runtime.RetryCount >= maxRetries {
			w.Runtime.LastError = fmt.Sprintf("delivery failed %d times, giving up on this batch: %s",
				w.Runtime.RetryCount, err)
			w.Runtime.PendingEvents = nil
			w.Runtime.RetryCount = 0
			w.Runtime.NextRetryAt = time.Time{}
			return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
		}
		w.Runtime.NextRetryAt = time.Now().Add(backoffFor(w.Runtime.RetryCount))
		w.Runtime.LastError = fmt.Sprintf("delivery attempt %d failed: %s", w.Runtime.RetryCount, err)
		return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
	}
	if delivered == 0 {
		return nil // every wake-worthy route lacks a holder: an operator condition, events kept
	}
	if len(keep) == 0 {
		w.Runtime.PendingSince = time.Time{}
	}
	w.Runtime.RetryCount = 0
	w.Runtime.NextRetryAt = time.Time{}
	w.Runtime.LastDelivered = time.Now().UTC()
	w.Runtime.DeliveredRound = w.Runtime.Round
	if w.PingEvery() > 0 {
		w.Runtime.LastPing = w.Runtime.LastDelivered
	}
	return nil
}

// routeGroup is one wake: the role it goes to and its events.
type routeGroup struct {
	target string
	events []model.Event
}

// routeGroups splits events by their trigger route, in first-seen order;
// an event with no route goes to def, the critic's alert target.
func routeGroups(events []model.Event, def string) []routeGroup {
	var out []routeGroup
	at := map[string]int{}
	for _, e := range events {
		t := e.Route
		if t == "" {
			t = def
		}
		i, ok := at[t]
		if !ok {
			i = len(out)
			at[t] = i
			out = append(out, routeGroup{target: t})
		}
		out[i].events = append(out[i].events, e)
	}
	return out
}

// PingHeld says a collect-mode critic must keep its wake-worthy events for
// a later ping: the last ping was less than PingEvery ago. Collection does not
// wait; only the ping does (W7 item 2: wake means "include in the next ping").
func PingHeld(w *model.Critic, now time.Time) bool {
	every := w.PingEvery()
	return every > 0 && !w.Runtime.LastPing.IsZero() && now.Sub(w.Runtime.LastPing) < every
}

// PingLine is a collect-mode wake: one line, the count and since when; the
// seat reads the inbox on the form it studies.
func PingLine(w *model.Critic) string {
	since := w.Runtime.LastPing
	if since.IsZero() {
		since = w.Runtime.PendingSince
	}
	n := len((&model.Delta{Events: w.Runtime.PendingEvents}).Tier1())
	return fmt.Sprintf("ebac %s: %d new since %s, in your studied form %s key inbox. When finished: ebac ack --critic %s --round %d",
		w.Name, n, since.UTC().Format(time.RFC3339), w.FormID, w.Name, w.Runtime.Round)
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

// MaybeArchive keeps a PR critic's AllClosedSince: set when every PR is
// closed or merged, cleared when one reopens. It archives nothing itself:
// ebacd ends every critic through one path (End), and the all-closed end
// is one of its reasons (model.Critic.EndReason).
func (p *Poller) MaybeArchive(ctx context.Context, w *model.Critic, snap *model.Snapshot) error {
	if !w.Stop.UntilAllClosed || snap == nil || len(snap.PRs) == 0 {
		return nil
	}
	allClosed := true
	for _, pr := range snap.PRs {
		if !pr.Closed() {
			allClosed = false
			break
		}
	}
	switch {
	case !allClosed && w.Runtime.AllClosedSince != nil:
		w.Runtime.AllClosedSince = nil
		return p.Store.SaveCritic(ctx, w)
	case allClosed && w.Runtime.AllClosedSince == nil:
		now := time.Now().UTC()
		w.Runtime.AllClosedSince = &now
		fmt.Fprintf(p.Out, "%s: all PRs closed; it ends after a %s grace\n", w.Name, time.Duration(w.Stop.ArchiveGraceSec)*time.Second)
		return p.Store.SaveCritic(ctx, w)
	}
	return nil
}

// EndNotify is told of every critic's end and every advance warning, beside
// the critic's own seat or alert role (ebac-owner, 11-19).
var EndNotify = "@000000a1d194a43d9fe2e32d96b12a83"

// notifyEnd sends one line to the critic's seat or alert role and to EndNotify.
func (p *Poller) notifyEnd(ctx context.Context, w *model.Critic, msg string) {
	targets := []string{}
	if t := w.AlertTarget(); t != "" {
		targets = append(targets, t)
	}
	if EndNotify != "" {
		targets = append(targets, EndNotify)
	}
	send := p.Send
	if send == nil {
		send = p.Fig.Send
	}
	for _, t := range targets {
		if err := send(ctx, t, msg); err != nil {
			fmt.Fprintf(p.Out, "%s: could not notify %s: %v\n", w.Name, t, err)
		}
	}
}

// End ends a critic for reason: one closing line to its seat or alert role
// and to EndNotify, the archive on disk, the critic marked archived, then its
// forms killed through the lifetime broker, children first and the role form
// last. Called again for a critic already ended (EndedAt set), it only retries
// the kills that failed: the critic is never polled once archived.
func (p *Poller) End(ctx context.Context, w *model.Critic, reason string) error {
	if w.Runtime.EndedAt == nil && w.Runtime.Archived {
		// Already archived (by hand, or before ends existed): its purge date
		// came. Nothing to tell or archive again; record the end, then kill.
		now := time.Now().UTC()
		w.Runtime.EndedAt = &now
		if err := p.Store.SaveCritic(ctx, w); err != nil {
			return fmt.Errorf("purge %s: %w", w.Name, err)
		}
		fmt.Fprintf(p.Out, "%s: PURGED (%s)\n", w.Name, reason)
	}
	if w.Runtime.EndedAt == nil {
		now := time.Now().UTC()
		snap, _ := p.Store.LoadSnapshot(ctx, w.Name)
		w.Runtime.Archived, w.Runtime.Stopped = true, true
		w.Runtime.ArchivedAt, w.Runtime.EndedAt = now, &now
		w.Runtime.StopReason = "ended: " + reason
		p.notifyEnd(ctx, w, fmt.Sprintf("ebac %s ended (%s); its forms are removed. Final state: %s", w.Name, reason, summariseFinal(snap)))
		dir, err := p.Store.Archive(ctx, w, snap)
		if err != nil {
			return fmt.Errorf("end %s: archive: %w", w.Name, err)
		}
		if err := p.Store.SaveCritic(ctx, w); err != nil {
			return fmt.Errorf("end %s: %w", w.Name, err)
		}
		fmt.Fprintf(p.Out, "%s: ENDED (%s), archived to %s\n", w.Name, reason, dir)
	}
	kill := p.Kill
	if kill == nil {
		kill = p.Fig.BrokerKill
	}
	keys := make([]string, 0, len(w.Runtime.Children))
	for k := range w.Runtime.Children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var failed []string
	for _, k := range keys {
		if err := kill(ctx, w.Runtime.Children[k]); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("end %s: %d child form(s) not removed (retried at the next listing): %s", w.Name, len(failed), strings.Join(failed, "; "))
	}
	if err := kill(ctx, w.FormID); err != nil {
		return fmt.Errorf("end %s: role form %s not removed (retried at the next listing): %w", w.Name, w.FormID, err)
	}
	fmt.Fprintf(p.Out, "%s: forms removed (%d child, role %s)\n", w.Name, len(keys), w.FormID)
	return nil
}

// WarnEnd sends the one advance warning of an expiry at at, and records it.
func (p *Poller) WarnEnd(ctx context.Context, w *model.Critic, at time.Time) error {
	p.notifyEnd(ctx, w, fmt.Sprintf("ebac %s expires %s (in %s): renew it with `ebac set --critic %s --renew`, or let it end.",
		w.Name, at.UTC().Format(time.RFC3339), time.Until(at).Round(time.Hour), w.Name))
	w.Runtime.EndWarnedFor = &at
	return p.Store.SaveCritic(ctx, w)
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

// refuseNoAuthority stops a round of a critic with no trusted authority
// before any plugin runs (#11).
func refuseNoAuthority(w *model.Critic) error {
	if w.NoAuthority {
		return fmt.Errorf("%s: %s", w.Name, model.NoAuthorityNote(w.FormID))
	}
	return nil
}
