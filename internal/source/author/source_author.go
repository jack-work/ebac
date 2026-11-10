package author

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// authorWatch follows one GitHub user's pull requests in a list of repos:
// it records the author's most recently updated PRs since the window start
// (at most AuthorRef.Window(), default 50, across all repos) on the form (key
// authored, ids and short fields only) and wakes the alert role on a
// NEW non-draft PR (a draft turning ready counts as new) or a MERGE. Close,
// reopen, head moves and new drafts are recorded, never woken. Asked by the
// Boss through the Decisions Minister, 2026-10-22.
//
// It is the github-author half of ebac-gh.
type Plugin struct {
	// GH, when set, is used as is (tests); otherwise each request builds a
	// client from the core's --gh/--host, retargeted to the critic's host.
	GH *forge.GH
	// Files, when set, replaces gh.PRFiles (tests).
	Files FilesFunc
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	gh := pl.GH
	if gh == nil {
		gh = forge.NewGH(req.GH, req.Host)
	}
	if req.Critic.Host != "" {
		gh.Host = req.Critic.Host // the token follows the host
	}
	switch req.Op {
	case plugin.OpRound:
		files := pl.Files
		if files == nil {
			files = gh.PRFiles
		}
		return round(ctx, gh, files, &req.Critic, req.Keys)
	case plugin.OpBrief:
		return &plugin.Reply{Brief: authorBrief(&req.Critic, plugin.Unwire(req.Pending))}, nil
	}
	return nil, fmt.Errorf("op %q not supported", req.Op)
}

const (
	EvAuthorNew      model.EventKind = "author_pr_new"
	EvAuthorMerged   model.EventKind = "author_pr_merged"
	EvAuthorDraft    model.EventKind = "author_pr_draft"
	EvAuthorClosed   model.EventKind = "author_pr_closed"
	EvAuthorReopened model.EventKind = "author_pr_reopened"
	EvAuthorHead     model.EventKind = "author_pr_head_moved"
)

// authorDiff compares this round's PRs with what was seen. The first round
// only records: a watch that starts by announcing a week of PRs is noise.
//
// Seen covers only the recent window, so a PR may be unseen because it
// re-entered the window (updated again) rather than because it is new: an
// unseen PR is NEW, or a fresh MERGE, only if it happened after the previous
// good round (prevGood); otherwise it is adopted silently.
//
// promoted are PRs the relevance filter skipped before and tracks now: news
// whenever they were created, because they were never shown.
func authorDiff(st *model.AuthorState, prs []forge.AuthoredPR, prevGood time.Time, promoted map[string]bool) []model.Event {
	first := st.Seen == nil
	if first {
		st.Seen = map[string]model.AuthorSeen{}
	}
	var events []model.Event
	ev := func(kind model.EventKind, tier model.Tier, pr forge.AuthoredPR) {
		events = append(events, model.Event{Kind: kind, Tier: tier, PR: model.PRKey(pr.Key()), URL: pr.URL, Detail: pr.Title})
	}
	for _, pr := range prs {
		k := pr.Key()
		prev, seen := st.Seen[k]
		st.Seen[k] = model.AuthorSeen{State: pr.State, Draft: pr.Draft, Head: pr.Head}
		if first {
			continue
		}
		if !seen {
			if !promoted[k] && !after(pr.Created, prevGood) && !(pr.State == "MERGED" && after(pr.Merged, prevGood)) {
				continue // re-entered the recent window: not news
			}
			switch {
			case pr.State == "MERGED":
				ev(EvAuthorMerged, model.TierWake, pr) // opened and merged between two rounds
			case pr.Draft:
				ev(EvAuthorDraft, model.TierRecord, pr)
			case pr.State == "OPEN":
				ev(EvAuthorNew, model.TierWake, pr)
			default:
				ev(EvAuthorClosed, model.TierRecord, pr)
			}
			continue
		}
		switch {
		case prev.State != "MERGED" && pr.State == "MERGED":
			ev(EvAuthorMerged, model.TierWake, pr)
		case prev.State != "CLOSED" && pr.State == "CLOSED":
			ev(EvAuthorClosed, model.TierRecord, pr)
		case prev.State != "OPEN" && pr.State == "OPEN":
			ev(EvAuthorReopened, model.TierRecord, pr)
		}
		if pr.State == "OPEN" && prev.Draft && !pr.Draft {
			ev(EvAuthorNew, model.TierWake, pr) // a draft turning ready counts as new
		}
		if prev.Head != "" && pr.Head != prev.Head && pr.State == "OPEN" {
			ev(EvAuthorHead, model.TierRecord, pr)
		}
	}
	return events
}

func round(ctx context.Context, gh *forge.GH, files FilesFunc, w *model.Critic, keys map[string]json.RawMessage) (*plugin.Reply, error) {
	rep := &plugin.Reply{Critic: w}
	a := w.Author
	if a == nil || a.Login == "" || len(a.Repos) == 0 {
		return nil, fmt.Errorf("%s: author watch without a login or repos", w.Name)
	}
	if a.Until != nil && time.Now().After(*a.Until) {
		w.Runtime.Stopped = true
		w.Runtime.StopReason = "window ended " + a.Until.UTC().Format(time.RFC3339)
		rep.DisableTimer, rep.Done = true, true
		rep.Logf("%s: %s", w.Name, w.Runtime.StopReason)
		return rep, nil
	}
	if w.Runtime.Author == nil {
		w.Runtime.Author = &model.AuthorState{}
	}

	n := a.Window()
	var prs []forge.AuthoredPR
	for _, repo := range a.Repos {
		got, err := gh.ListAuthoredPRs(ctx, repo, a.Login, a.Since, n)
		if err != nil {
			w.Runtime.ConsecutiveFailures++
			w.Runtime.LastError = err.Error()
			rep.Error = err.Error()
			return rep, nil
		}
		prs = append(prs, got...)
	}
	prs = recentWindow(prs, n)

	st := w.Runtime.Author
	f, spec, ferr := resolveFilter(st, keys)
	if ferr != st.FilterErr {
		if ferr == "" {
			rep.Unset = []string{model.AuthorFilterErr}
		} else {
			rep.Set = map[string]json.RawMessage{model.AuthorFilterErr: model.MustJSON(ferr)}
			rep.Logf("%s: %s", w.Name, ferr)
		}
		st.FilterErr = ferr
	}
	prs, promoted, err := relevant(ctx, st, f, spec, prs, files)
	if err != nil {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	prevGood := w.Runtime.LastSuccess
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = time.Now().UTC()

	rep.Events = plugin.Wire(authorDiff(st, prs, prevGood, promoted))
	// Seen and the authored key hold the tracked window and nothing else: what left it (older, merged/closed long
	// ago, or untracked by the filter) is dropped, which also prunes a form written before the window or the
	// filter existed on its first round.
	keep := map[string]bool{}
	for _, pr := range prs {
		keep[pr.Key()] = true
	}
	for k := range st.Seen {
		if !keep[k] {
			delete(st.Seen, k)
		}
	}
	view := authoredView(prs)
	raw := model.MustJSON(view)
	if d := digest(raw); d != st.Window {
		if rep.Set == nil {
			rep.Set = map[string]json.RawMessage{}
		}
		rep.Set["authored"] = raw
		st.Window = d
	}
	return rep, nil
}

// recentWindow is the n most recently updated PRs across every repo, sorted by key.
func recentWindow(prs []forge.AuthoredPR, n int) []forge.AuthoredPR {
	sort.SliceStable(prs, func(i, j int) bool {
		if prs[i].Updated != prs[j].Updated {
			return prs[i].Updated > prs[j].Updated // RFC 3339 UTC sorts as text
		}
		return prs[i].Key() < prs[j].Key()
	})
	if len(prs) > n {
		prs = prs[:n]
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Key() < prs[j].Key() })
	return prs
}

// after says whether ts is at or after t. With no cutoff, or a time that
// cannot be read, it says yes: an unseen PR is news unless shown otherwise.
func after(ts string, t time.Time) bool {
	if t.IsZero() || ts == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, ts)
	return err != nil || !at.Before(t)
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func authorBrief(w *model.Critic, events []model.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ebac %s round %d: PRs by %s.\n", w.Name, w.Runtime.Round, w.Author.Login)
	for _, e := range events {
		if e.Tier != model.TierWake {
			continue
		}
		what := "NEW"
		if e.Kind == EvAuthorMerged {
			what = "MERGED"
		}
		fmt.Fprintf(&b, "- %s %s %s %s\n", what, e.PR, e.URL, model.FirstLine(e.Detail))
	}
	if n := len(events) - countWake(events); n > 0 {
		fmt.Fprintf(&b, "(%d background change(s): closed, reopened, head moved or new draft.)\n", n)
	}
	fmt.Fprintf(&b, "Every tracked PR is in form %s, key authored. When finished: ebac ack --critic %s --round %d\n",
		w.FormID, w.Name, w.Runtime.Round)
	return b.String()
}

func countWake(events []model.Event) int {
	n := 0
	for _, e := range events {
		if e.Tier == model.TierWake {
			n++
		}
	}
	return n
}

// authoredView is the authored key: the recent window, by PR key.
func authoredView(prs []forge.AuthoredPR) map[string]forge.AuthoredPR {
	out := map[string]forge.AuthoredPR{}
	for _, pr := range prs {
		out[pr.Key()] = pr
	}
	return out
}
