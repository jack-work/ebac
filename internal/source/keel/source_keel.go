package keel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jack-work/figaro/api/handle"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/systemd"
)

// Source translates one keel repo's comment threads into a critic's inbox and
// wakes the seat when the waited-on author answers, like GitHub threads. It
// only reads: the door forwards GET threads and nothing else.
type Source struct{}

func (Source) Name() string { return "keel-threads" }

func (Source) Round(ctx context.Context, p *poll.Poller, w *model.Critic) error {
	return pollKeel(ctx, p, w, ForRef(w.Keel))
}

const (
	EvKeelNew      model.EventKind = "keel_new"
	EvKeelEdited   model.EventKind = "keel_edited"
	EvKeelResolved model.EventKind = "keel_resolved"
	EvKeelReopened model.EventKind = "keel_reopened"

	keelBodyBytes  = 2000
	keelQuoteBytes = 300
	keelGoneAfter  = 3 // consecutive "gone" answers before the repo is declared gone
)

var feedWait = FeedWait

const (
	FeedWait     = 30 * time.Second
	FeedEvery    = 45 * time.Second
	ThreadsEvery = 5 * time.Minute
)

func MinEvery(feed bool) time.Duration {
	if feed {
		return FeedWait + 5*time.Second
	}
	return 0
}

// keelBlindAfter is how long the watcher may fail before it tells the seat it
// is not hearing keel. A var so a test can shorten it.
var keelBlindAfter = 10 * time.Minute

// IndexEntry is one thread on the form's lean index (key threads): no bodies.
type IndexEntry struct {
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Resolved bool   `json:"resolved,omitempty"`
	N        int    `json:"n"`
	LastBy   string `json:"last_by,omitempty"`
}

func nametagAccount(n string) string {
	if _, account, ok := strings.Cut(n, " ⟩ "); ok {
		return account
	}
	return n
}

func exactly(e FeedEvent, nametags []string) bool {
	for _, want := range nametags {
		want = strings.TrimSpace(want)
		if want != "" && e.Author != "" && e.Signed == want && e.Author == nametagAccount(want) {
			return true
		}
	}
	return false
}

func nametagAccounts(ref *model.KeelRef) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range append(append([]string{}, ref.Self...), ref.WaitingOn...) {
		if a := nametagAccount(strings.TrimSpace(n)); a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func seatNametags(seat string, accounts []string) []string {
	seat = strings.TrimPrefix(strings.TrimSpace(seat), "@")
	if seat == "" {
		return nil
	}
	names := []string{"Figaro", "Figaro " + seat}
	if len(seat) > 8 {
		names = append(names, "Figaro "+seat[:8])
	}
	if h := handle.Words(seat, handle.MinWords); h != "" {
		names = append(names, "Figaro "+h)
	}
	var out []string
	for _, a := range accounts {
		for _, n := range names {
			out = append(out, n+" ⟩ "+a)
		}
	}
	return out
}

func withSeat(ref *model.KeelRef, seat string) *model.KeelRef {
	r := *ref
	r.Self = append(append([]string{}, ref.Self...), seatNametags(seat, nametagAccounts(ref))...)
	return &r
}

func seatOf(ctx context.Context, p *poll.Poller, w *model.Critic) string {
	f, err := p.Fig.Form(ctx, w.AlertTarget())
	if err != nil {
		return ""
	}
	id, _ := f["target-aria"].(string)
	return id
}

func shouldWake(e FeedEvent, ref *model.KeelRef) bool {
	return exactly(e, ref.WaitingOn) || !knownPrincipal(e.Author, ref)
}

func knownPrincipal(author string, ref *model.KeelRef) bool {
	for _, n := range append(append([]string{}, ref.Self...), ref.WaitingOn...) {
		if n = strings.TrimSpace(n); n != "" && nametagAccount(n) == author {
			return true
		}
	}
	return false
}

// pathMatches applies the critic's globs: path.Match, and dir/** for a subtree.
func pathMatches(p string, globs []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if dir, ok := strings.CutSuffix(g, "/**"); ok && strings.HasPrefix(p, dir+"/") {
			return true
		}
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

// keelDiff compares a fetch with what was seen. The first round only records.
// A new or edited comment by a waited-on author in an open thread wakes; any
// other comment and a resolve or reopen are recorded; the seat's own comments
// are never announced. Seen is rebuilt from the fetch, so it never outgrows it.
func keelDiff(st *model.KeelState, ref *model.KeelRef, byPath map[string][]Thread) (events []model.Event, items map[string]form.InboxItem, index map[string]IndexEntry) {
	items, index = map[string]form.InboxItem{}, map[string]IndexEntry{}
	first := st.Seen == nil
	seen, resolved := map[string]string{}, map[string]bool{}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	repo := model.PRKey(ref.Repo)
	for _, p := range paths {
		if !pathMatches(p, ref.Paths) {
			continue
		}
		for _, t := range byPath[p] {
			if t.ID == "" {
				continue
			}
			ix := indexEntry(p, t)
			line := ix.Line
			index[t.ID] = ix
			resolved[t.ID] = t.Resolved
			if was, known := st.Resolved[t.ID]; !first && known && was != t.Resolved {
				kind := EvKeelReopened
				if t.Resolved {
					kind = EvKeelResolved
				}
				events = append(events, model.Event{Kind: kind, Tier: model.TierRecord, PR: repo, ThreadID: t.ID, Path: p})
			}
			for _, c := range t.Comments {
				if c.ID == "" {
					continue
				}
				d := model.BodyDigest(c.Body)
				prev, had := st.Seen[c.ID]
				seen[c.ID] = d
				who := FeedEvent{Signed: c.Author, Author: c.Account}
				if first || prev == d || exactly(who, ref.Self) {
					continue
				}
				kind := EvKeelNew
				if had {
					kind = EvKeelEdited
				}
				tier := model.TierRecord
				if (!t.Resolved || !had) && shouldWake(who, ref) {
					tier = model.TierWake
				}
				body, cut := model.CutBody(c.Body, keelBodyBytes)
				quote, _ := model.CutBody(t.Quote, keelQuoteBytes)
				events = append(events, model.Event{Kind: kind, Tier: tier, PR: repo, ThreadID: t.ID, Comment: c.ID, Author: c.Author,
					Path: p, Detail: model.FirstLine(body)})
				items["keel-"+c.ID] = form.InboxItem{Kind: string(kind), By: c.Author, Time: c.Created.UTC().Format(time.RFC3339),
					Thread: t.ID, Path: p, Line: line, Quote: quote, Body: body, Cut: cut,
					Fetch: fmt.Sprintf("keel %s, thread %s: GET %s | jq '.paths[%q][] | select(.id==%q)'", ref.Repo, t.ID, ThreadsPath(ref.Repo), p, t.ID)}
			}
		}
	}
	st.Seen, st.Resolved = seen, resolved
	return events, items, index
}

func indexEntry(p string, t Thread) IndexEntry {
	ix := IndexEntry{Path: p, Resolved: t.Resolved, N: len(t.Comments)}
	if t.Anchor != nil {
		ix.Line = t.Anchor.StartLine
	}
	if n := len(t.Comments); n > 0 {
		ix.LastBy = t.Comments[n-1].Author
	}
	return ix
}

func buildIndex(ref *model.KeelRef, byPath map[string][]Thread) map[string]IndexEntry {
	index := map[string]IndexEntry{}
	for p, ts := range byPath {
		if !pathMatches(p, ref.Paths) {
			continue
		}
		for _, t := range ts {
			if t.ID != "" {
				index[t.ID] = indexEntry(p, t)
			}
		}
	}
	return index
}

type keelRound struct {
	events []model.Event
	items  map[string]form.InboxItem
	index  map[string]IndexEntry
}

func fetchRound(ctx context.Context, cl *Client, st *model.KeelState, ref *model.KeelRef) (keelRound, error) {
	if !ref.Feed {
		byPath, err := cl.Threads(ctx, ref.Repo)
		if err != nil {
			return keelRound{}, err
		}
		ev, items, index := keelDiff(st, ref, byPath)
		return keelRound{ev, items, index}, nil
	}
	if !st.FeedStarted {
		head, err := feedHead(ctx, cl, ref.Repo)
		if err != nil {
			return keelRound{}, err
		}
		byPath, err := cl.Threads(ctx, ref.Repo)
		if err != nil {
			return keelRound{}, err
		}
		ev, items, index := keelDiff(st, ref, byPath)
		st.Cursor, st.FeedStarted = head, true
		st.Seen, st.Resolved = nil, nil
		return keelRound{ev, items, index}, nil
	}
	evs, next, err := cl.Events(ctx, ref.Repo, st.Cursor, feedWait)
	if err != nil {
		return keelRound{}, err
	}
	if len(evs) == 0 {
		st.Cursor = next
		return keelRound{}, nil
	}
	byPath, err := cl.Threads(ctx, ref.Repo)
	if err != nil {
		return keelRound{}, err
	}
	r := feedDiff(st.Cursor, ref, evs, byPath)
	st.Cursor = next
	return r, nil
}

func feedHead(ctx context.Context, cl *Client, repo string) (int64, error) {
	var after int64
	for {
		evs, next, err := cl.Events(ctx, repo, after, 0)
		if err != nil {
			return 0, err
		}
		if len(evs) == 0 {
			return next, nil
		}
		after = next
	}
}

func feedDiff(after int64, ref *model.KeelRef, evs []FeedEvent, byPath map[string][]Thread) keelRound {
	type found struct {
		path string
		t    Thread
		c    Comment
	}
	byComment := map[string]found{}
	for p, ts := range byPath {
		for _, t := range ts {
			for _, c := range t.Comments {
				byComment[c.ID] = found{p, t, c}
			}
		}
	}
	r := keelRound{items: map[string]form.InboxItem{}, index: buildIndex(ref, byPath)}
	repo := model.PRKey(ref.Repo)
	done := map[string]bool{}
	for _, e := range evs {
		if e.Seq <= after || !pathMatches(e.Path, ref.Paths) {
			continue
		}
		switch e.Kind {
		case "thread.resolved", "thread.reopened":
			kind := EvKeelReopened
			if e.Kind == "thread.resolved" {
				kind = EvKeelResolved
			}
			r.events = append(r.events, model.Event{Kind: kind, Tier: model.TierRecord, PR: repo, ThreadID: e.Thread, Path: e.Path})
		case "comment.created", "comment.edited":
			x, ok := byComment[e.Comment]
			if !ok || done[e.Comment] || exactly(e, ref.Self) {
				continue
			}
			done[e.Comment] = true
			kind := EvKeelNew
			if e.Kind == "comment.edited" {
				kind = EvKeelEdited
			}
			tier := model.TierRecord
			if (!x.t.Resolved || e.Kind == "comment.created") && shouldWake(e, ref) {
				tier = model.TierWake
			}
			line := indexEntry(x.path, x.t).Line
			body, cut := model.CutBody(x.c.Body, keelBodyBytes)
			quote, _ := model.CutBody(x.t.Quote, keelQuoteBytes)
			r.events = append(r.events, model.Event{Kind: kind, Tier: tier, PR: repo, ThreadID: x.t.ID, Comment: x.c.ID, Author: e.Signed,
				Path: x.path, Detail: model.FirstLine(body)})
			r.items["keel-"+x.c.ID] = form.InboxItem{Kind: string(kind), By: e.Signed, Time: x.c.Created.UTC().Format(time.RFC3339),
				Thread: x.t.ID, Path: x.path, Line: line, Quote: quote, Body: body, Cut: cut,
				Fetch: fmt.Sprintf("keel %s, thread %s: GET %s | jq '.paths[%q][] | select(.id==%q)'", ref.Repo, x.t.ID, ThreadsPath(ref.Repo), x.path, x.t.ID)}
		}
	}
	return r
}

func pollKeel(ctx context.Context, p *poll.Poller, w *model.Critic, cl *Client) error {
	if w.Keel == nil || w.Keel.Repo == "" {
		return fmt.Errorf("%s: keel critic without a repo", w.Name)
	}
	if w.Runtime.Keel == nil {
		w.Runtime.Keel = &model.KeelState{}
	}
	st := w.Runtime.Keel
	w.Runtime.Round++
	w.Runtime.LastPolled = time.Now().UTC()

	round, err := fetchRound(ctx, cl, st, withSeat(w.Keel, seatOf(ctx, p, w)))
	if err != nil {
		if errors.Is(err, ErrGone) {
			st.GoneStreak++
			if st.GoneSince == "" {
				st.GoneSince = time.Now().UTC().Format(time.RFC3339)
			}
		} else {
			st.GoneStreak = 0
		}
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		if st.FailSince == "" {
			st.FailSince = time.Now().UTC().Format(time.RFC3339)
		}
		if st.GoneStreak >= keelGoneAfter {
			return keelGone(ctx, p, w, err)
		}
		keelBlind(ctx, p, w, err)
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	st.GoneStreak, st.GoneSince = 0, ""
	if st.Blind {
		if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
			return nil, []string{"blind"}
		}); err != nil {
			return fmt.Errorf("%s: clear blind marker: %w", w.Name, err)
		}
		fmt.Fprintf(p.Out, "%s: hearing keel again (blind since %s)\n", w.Name, st.FailSince)
	}
	st.FailSince, st.Blind, st.BlindErr = "", false, ""
	w.Runtime.ConsecutiveFailures = 0
	w.Runtime.LastError = ""
	w.Runtime.LastSuccess = time.Now().UTC()

	events, items, index := round.events, round.items, round.index
	now := time.Now().UTC().Format(time.RFC3339)
	for k, it := range items {
		it.At = now
		items[k] = it
	}
	if len(events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = time.Now().UTC()
		}
		w.Runtime.PendingEvents = poll.MergeEvents(w.Runtime.PendingEvents, events)
	}
	if err := p.Store.SaveRound(ctx, w, nil, nil, items); err != nil {
		return err
	}
	if index != nil {
		if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
			return map[string]json.RawMessage{"threads": model.MustJSON(index)}, nil
		}); err != nil {
			return fmt.Errorf("%s: write thread index: %w", w.Name, err)
		}
	}
	figaro.Renew(ctx, p.Fig, w.FormID)

	derr := deliverKeel(ctx, p, w)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	return derr
}

func deliverKeel(ctx context.Context, p *poll.Poller, w *model.Critic) error {
	pending := &model.Delta{Events: w.Runtime.PendingEvents}
	if !pending.WakeWorthy() {
		fmt.Fprintf(p.Out, "%s: round %d quiet (%d recorded)\n", w.Name, w.Runtime.Round, len(w.Runtime.PendingEvents))
		w.Runtime.PendingEvents, w.Runtime.PendingSince = nil, time.Time{}
		return nil
	}
	target := w.AlertTarget()
	if !p.RoleHasHolder(ctx, target) {
		w.Runtime.LastError = "role has no reachable target-aria; cast a figaro into " + target
		fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.LastError)
		return nil
	}
	if err := p.Fig.Send(ctx, target, keelBrief(w, w.Runtime.PendingEvents)); err != nil {
		w.Runtime.RetryCount++
		w.Runtime.LastError = fmt.Sprintf("delivery attempt %d failed: %v", w.Runtime.RetryCount, err)
		return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
	}
	fmt.Fprintf(p.Out, "%s: alerted %s — %d event(s)\n", w.Name, target, len(w.Runtime.PendingEvents))
	w.Runtime.PendingEvents, w.Runtime.PendingSince = nil, time.Time{}
	w.Runtime.RetryCount = 0
	w.Runtime.LastDelivered = time.Now().UTC()
	w.Runtime.DeliveredRound = w.Runtime.Round
	return nil
}

func keelBrief(w *model.Critic, events []model.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ebac %s round %d: answered on keel %s.\n", w.Name, w.Runtime.Round, w.Keel.Repo)
	wake := 0
	for _, e := range events {
		if e.Tier != model.TierWake {
			continue
		}
		wake++
		what := "NEW"
		if e.Kind == EvKeelEdited {
			what = "EDITED"
		}
		fmt.Fprintf(&b, "- %s by %s on %s, thread %s: %s\n", what, e.Author, e.Path, e.ThreadID, e.Detail)
	}
	if n := len(events) - wake; n > 0 {
		fmt.Fprintf(&b, "(%d background change(s): other authors, resolves, reopens.)\n", n)
	}
	fmt.Fprintf(&b, "Comments are in form %s, key inbox (cut at %d B); key threads indexes every thread. "+
		"When finished: ebac ack --critic %s --round %d\n", w.FormID, keelBodyBytes, w.Name, w.Runtime.Round)
	return b.String()
}

// wake tells the alert target once; a role with nobody seated is skipped.
func wake(ctx context.Context, p *poll.Poller, w *model.Critic, msg string) {
	target := w.AlertTarget()
	if !p.RoleHasHolder(ctx, target) {
		return
	}
	if err := p.Fig.Send(ctx, target, msg); err != nil {
		fmt.Fprintf(p.Out, "%s: could not deliver: %v\n", w.Name, err)
	}
}

// keelBlind writes the form's key blind once failures outlast keelBlindAfter,
// wakes the seat the first time, and is cleared by the first good round.
func keelBlind(ctx context.Context, p *poll.Poller, w *model.Critic, cause error) {
	st := w.Runtime.Keel
	since, err := time.Parse(time.RFC3339, st.FailSince)
	if err != nil || time.Since(since) < keelBlindAfter {
		return
	}
	if st.Blind && st.BlindErr == cause.Error() {
		return
	}
	marker := map[string]string{"since": st.FailSince, "last_error": cause.Error(), "updated": time.Now().UTC().Format(time.RFC3339),
		"meaning": "ebac cannot read keel threads of " + w.Keel.Repo + ": comments since then are unseen until this key disappears"}
	if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{"blind": model.MustJSON(marker)}, nil
	}); err != nil {
		fmt.Fprintf(p.Out, "%s: could not write blind marker: %v\n", w.Name, err)
		return
	}
	if !st.Blind {
		wake(ctx, p, w, fmt.Sprintf("ebac %s: BLIND since %s: cannot read keel %s (%v). Comments are unseen until form %s's key blind disappears; it clears itself on recovery.",
			w.Name, st.FailSince, w.Keel.Repo, cause, w.FormID))
		fmt.Fprintf(p.Out, "%s: blind since %s: %v\n", w.Name, st.FailSince, cause)
	}
	st.Blind, st.BlindErr = true, cause.Error()
}

// keelGone marks the repo gone on the form, wakes the seat once and stops.
func keelGone(ctx context.Context, p *poll.Poller, w *model.Critic, cause error) error {
	st := w.Runtime.Keel
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("keel threads gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. If the repo is not on ebac's keel allowlist (ebacThreadRepos) or the door is down, fix that, then " +
		"ebac resume --critic " + w.Name + " and re-arm its timer; otherwise remove it: ebac rm --critic " + w.Name
	if err := p.MarkGone(ctx, w, "keel", "keel threads of "+w.Keel.Repo, st.GoneSince, cause, action); err != nil {
		return err
	}
	wake(ctx, p, w, fmt.Sprintf("ebac %s: keel threads of %s are gone (%v). Polling stopped; form %s's key gone says what to do.", w.Name, w.Keel.Repo, cause, w.FormID))
	_ = systemd.DisableTimer(w.Name)
	fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.StopReason)
	return p.Store.SaveCritic(ctx, w)
}
