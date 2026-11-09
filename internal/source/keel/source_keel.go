package keel

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jack-work/figaro/api/handle"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Source translates one keel repo's comment threads into a critic's inbox and
// wakes the seat when the waited-on author answers, like GitHub threads. It
// only reads: the door forwards GET threads and nothing else.
//
// It is the ebac-keel plugin.
type Plugin struct {
	// Client, when set, is used as is (tests); otherwise it is built from the
	// critic's keel ref.
	Client *Client
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	w := &req.Critic
	switch req.Op {
	case plugin.OpRound:
		if w.Keel == nil || w.Keel.Repo == "" {
			return nil, fmt.Errorf("%s: keel critic without a repo", w.Name)
		}
		cl := pl.Client
		if cl == nil {
			cl = ForRef(w.Keel)
		}
		return round(ctx, cl, w, req.Seat), nil
	case plugin.OpBrief:
		return &plugin.Reply{Brief: keelBrief(w, plugin.Unwire(req.Pending))}, nil
	}
	return nil, fmt.Errorf("op %q not supported", req.Op)
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
					Path: p, Detail: model.FirstLine(body), Text: c.Body})
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
				Path: x.path, Detail: model.FirstLine(body), Text: x.c.Body})
			r.items["keel-"+x.c.ID] = form.InboxItem{Kind: string(kind), By: e.Signed, Time: x.c.Created.UTC().Format(time.RFC3339),
				Thread: x.t.ID, Path: x.path, Line: line, Quote: quote, Body: body, Cut: cut,
				Fetch: fmt.Sprintf("keel %s, thread %s: GET %s | jq '.paths[%q][] | select(.id==%q)'", ref.Repo, x.t.ID, ThreadsPath(ref.Repo), x.path, x.t.ID)}
		}
	}
	return r
}

func round(ctx context.Context, cl *Client, w *model.Critic, seat string) *plugin.Reply {
	rep := &plugin.Reply{Critic: w}
	if w.Runtime.Keel == nil {
		w.Runtime.Keel = &model.KeelState{}
	}
	st := w.Runtime.Keel

	r, err := fetchRound(ctx, cl, st, withSeat(w.Keel, seat))
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
			keelGone(rep, w, err)
			return rep
		}
		keelBlind(rep, w, err)
		rep.Error = err.Error()
		return rep
	}
	st.GoneStreak, st.GoneSince = 0, ""
	if st.Blind {
		rep.Unset = append(rep.Unset, "blind")
		rep.Logf("%s: hearing keel again (blind since %s)", w.Name, st.FailSince)
	}
	st.FailSince, st.Blind, st.BlindErr = "", false, ""
	w.Runtime.ConsecutiveFailures = 0
	w.Runtime.LastError = ""
	w.Runtime.LastSuccess = time.Now().UTC()

	now := time.Now().UTC().Format(time.RFC3339)
	for k, it := range r.items {
		it.At = now
		r.items[k] = it
	}
	rep.Inbox = r.items
	rep.Events = plugin.Wire(r.events)
	for i, e := range r.events {
		if _, ok := r.items["keel-"+e.Comment]; ok {
			rep.Events[i].Inbox = []string{"keel-" + e.Comment}
		}
	}
	if r.index != nil {
		rep.SetKey("threads", r.index)
	}
	return rep
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

// keelBlind writes the form's key blind once failures outlast keelBlindAfter,
// wakes the seat the first time, and is cleared by the first good round.
func keelBlind(rep *plugin.Reply, w *model.Critic, cause error) {
	st := w.Runtime.Keel
	since, err := time.Parse(time.RFC3339, st.FailSince)
	if err != nil || time.Since(since) < keelBlindAfter {
		return
	}
	if st.Blind && st.BlindErr == cause.Error() {
		return
	}
	rep.SetKey("blind", map[string]string{"since": st.FailSince, "last_error": cause.Error(), "updated": time.Now().UTC().Format(time.RFC3339),
		"meaning": "ebac cannot read keel threads of " + w.Keel.Repo + ": comments since then are unseen until this key disappears"})
	if !st.Blind {
		rep.Wake = append(rep.Wake, fmt.Sprintf("ebac %s: BLIND since %s: cannot read keel %s (%v). Comments are unseen until form %s's key blind disappears; it clears itself on recovery.",
			w.Name, st.FailSince, w.Keel.Repo, cause, w.FormID))
		rep.Logf("%s: blind since %s: %v", w.Name, st.FailSince, cause)
	}
	st.Blind, st.BlindErr = true, cause.Error()
}

// keelGone marks the repo gone on the form, wakes the seat once and stops.
func keelGone(rep *plugin.Reply, w *model.Critic, cause error) {
	st := w.Runtime.Keel
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("keel threads gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. If the repo is not on ebac's keel allowlist (ebacThreadRepos) or the door is down, fix that, then " +
		"ebac resume --critic " + w.Name + " and re-arm its timer; otherwise remove it: ebac rm --critic " + w.Name
	rep.Gone = append(rep.Gone, plugin.Gone{Key: "keel", Resource: "keel threads of " + w.Keel.Repo, Since: st.GoneSince, Error: cause.Error(), Action: action})
	rep.Wake = append(rep.Wake, fmt.Sprintf("ebac %s: keel threads of %s are gone (%v). Polling stopped; form %s's key gone says what to do.", w.Name, w.Keel.Repo, cause, w.FormID))
	rep.DisableTimer, rep.Done = true, true
	rep.Logf("%s: %s", w.Name, w.Runtime.StopReason)
}
