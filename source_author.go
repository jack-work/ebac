package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/systemd"
)

// authorWatch follows one GitHub user's pull requests in a list of repos:
// it records every PR created or updated since the window start on the form
// (key authored, ids and short fields only) and wakes the alert role on a
// NEW non-draft PR (a draft turning ready counts as new) or a MERGE. Close,
// reopen, head moves and new drafts are recorded, never woken. Asked by the
// Boss through the Decisions Minister, 2026-10-22.
type authorWatch struct{}

func (authorWatch) Name() string { return "github-author" }

func (authorWatch) Round(ctx context.Context, p *poll.Poller, w *model.Critic) error {
	return pollAuthor(ctx, p, w)
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
func authorDiff(st *model.AuthorState, prs []forge.AuthoredPR) []model.Event {
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

func pollAuthor(ctx context.Context, p *poll.Poller, w *model.Critic) error {
	a := w.Author
	if a == nil || a.Login == "" || len(a.Repos) == 0 {
		return fmt.Errorf("%s: author watch without a login or repos", w.Name)
	}
	if a.Until != nil && time.Now().After(*a.Until) {
		w.Runtime.Stopped = true
		w.Runtime.StopReason = "window ended " + a.Until.UTC().Format(time.RFC3339)
		_ = systemd.DisableTimer(w.Name)
		fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.StopReason)
		return p.Store.SaveCritic(ctx, w)
	}
	if w.Host != "" {
		p.GH.Host = w.Host // the poller's client starts on its default host; the token follows the host
	}
	if w.Runtime.Author == nil {
		w.Runtime.Author = &model.AuthorState{}
	}
	w.Runtime.Round++
	w.Runtime.LastPolled = time.Now().UTC()

	var prs []forge.AuthoredPR
	for _, repo := range a.Repos {
		got, err := p.GH.ListAuthoredPRs(ctx, repo, a.Login, a.Since)
		if err != nil {
			w.Runtime.ConsecutiveFailures++
			w.Runtime.LastError = err.Error()
			_ = p.Store.SaveCritic(ctx, w)
			return fmt.Errorf("%s: %w", w.Name, err)
		}
		prs = append(prs, got...)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Key() < prs[j].Key() })
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = time.Now().UTC()

	events := authorDiff(w.Runtime.Author, prs)
	if len(events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = time.Now().UTC()
		}
		w.Runtime.PendingEvents = poll.MergeEvents(w.Runtime.PendingEvents, events)
	}
	authored := map[string]forge.AuthoredPR{}
	for _, pr := range prs {
		authored[pr.Key()] = pr
	}
	if err := p.Store.SaveRound(ctx, w, nil, nil, nil); err != nil {
		return err
	}
	if err := p.Store.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		next := model.MustJSON(authored)
		if string(cur["authored"]) == string(next) {
			return nil, nil
		}
		return map[string]json.RawMessage{"authored": next}, nil
	}); err != nil {
		return err
	}
	figaro.Renew(ctx, p.Fig, w.FormID)
	derr := deliverAuthor(ctx, p, w)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	return derr
}

func deliverAuthor(ctx context.Context, p *poll.Poller, w *model.Critic) error {
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
	if err := p.Fig.Send(ctx, target, authorBrief(w, w.Runtime.PendingEvents)); err != nil {
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
	fmt.Fprintf(&b, "Every PR is in form %s, key authored. When finished: ebac ack --critic %s --round %d\n",
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

// cmdAddAuthor mints an author watch: role form, critic, optional seat, timer.
func cmdAddAuthor(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-author")
	name := fs.String("name", "", "critic name, e.g. author-chiqun")
	login := fs.String("login", "", "GitHub login whose PRs to watch")
	since := fs.String("since", "", "window start, RFC 3339 (e.g. 2026-10-15T00:00:00Z)")
	until := fs.String("until", "", "window end, RFC 3339 (optional; the watch stops after it)")
	repos := fs.String("repo", "", "comma-separated owner/repo list")
	alert := fs.String("alert", "", "role to wake (@form); its target-aria is resolved at every send")
	aria := fs.String("aria", "", "seat cast into the critic's own role form (optional)")
	host := fs.String("host", "microsoft.ghe.com", "forge host")
	every := fs.Duration("every", 5*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if *login == "" || *repos == "" || *since == "" {
		return fmt.Errorf("--login, --repo and --since are required")
	}
	ref := &model.AuthorRef{Login: *login, AlertRole: *alert}
	var err error
	if ref.Since, err = time.Parse(time.RFC3339, *since); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if *until != "" {
		u, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		ref.Until = &u
	}
	for _, r := range strings.Split(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			if !strings.Contains(r, "/") {
				return fmt.Errorf("--repo %q: want owner/repo", r)
			}
			ref.Repos = append(ref.Repos, r)
		}
	}
	if err := forge.CheckForgeHost(*host); err != nil {
		return err
	}
	if ref.AlertRole != "" && !strings.HasPrefix(ref.AlertRole, "@") {
		return fmt.Errorf("--alert %q: want a role form (@id)", ref.AlertRole)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Mode: model.ModeAuthor, Source: "github-author", Host: *host, CreatedAt: time.Now().UTC(),
		FigaroSock: g.figaro, EverySec: int(every.Seconds()), Author: ref, Stop: model.StopConditions{}}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if *aria != "" {
		if err := model.ValidAriaID(*aria); err != nil {
			return fmt.Errorf("critic created (role %s), but --aria is bad: %w", formID, err)
		}
		if err := st.Fig.Cast(ctx, *aria, formID); err != nil {
			return fmt.Errorf("critic created (role %s), but casting %s failed: %w", formID, *aria, err)
		}
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("author watch %q: role %s, %s in %s since %s, alert %s, every %s\n", *name, formID, ref.Login,
		strings.Join(ref.Repos, ","), ref.Since.UTC().Format(time.RFC3339), w.AlertTarget(), *every)
	return nil
}
