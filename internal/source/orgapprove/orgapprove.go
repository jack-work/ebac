// Package orgapprove is ORG-APPROVE (the Boss, 2026-11-17): the open
// bic/aether pull requests of the Boss's org, read with ONE lean aliased
// search per round (1 point), deep-read only when they pass the cheap filters
// and their head moved (or the last deep read is DeepEvery old), scored for
// "stalled, ready for an approval", and asked: one band-grants request per
// candidate (principal org-approve, credential ebac-approve.org) and one
// q.oa-<pr>-<head7> on the elicitation board that links it. The /grants/
// approval of THAT request, by jack, is the tap: ebac then posts the APPROVE,
// after re-checking the PR at its head. Nothing else approves: an answer on
// the board does not.
package orgapprove

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/approve"
)

const (
	// Repo is the only repository read and approved in.
	Repo = approve.Repo
	// Self is Jack's GHE login: his own PRs are never asked, and a PR he
	// already reviewed is excluded.
	Self = "John-Kelliher"
	// MinAge, MinIterations: the stall floor.
	MinAge        = 48 * time.Hour
	MinIterations = 3
	// DeepEvery re-reads a cheap-filter passer whose head did not move
	// (threads get answered and resolved without a push).
	DeepEvery = 6 * time.Hour
	// DeepPerRound is one deep query (4 PRs measured ~5 points).
	DeepPerRound = 4
	// MaxOpenAsks is band-grants' max_pending per principal.
	MaxOpenAsks = 5
	// AskTTL is the grant each request asks for: the window to post in.
	AskTTL = 2 * time.Hour
	// PendingLapse is band-grants' pending_ttl_s: an unanswered request lapses.
	PendingLapse = 24 * time.Hour
	// KeepDone is how long a decided ask is kept in the critic's state.
	KeepDone = 7 * 24 * time.Hour
	// GrantsPage is where the Boss taps.
	GrantsPage = "https://cheroot.band.internal/grants/"
)

// Body is the review body (GHE text policy: names no internal tool but Figaro).
const Body = "Approved on Jack's behalf (Figaro), from his review queue."

// Reader is the GraphQL read surface: approve.GH.Query, reads only.
type Reader interface {
	Query(ctx context.Context, query string) (json.RawMessage, error)
	Spent() approve.Spend
}

// Writer is the REST surface of the post: approve.GH.
type Writer interface {
	PR(ctx context.Context, n int) (approve.PRInfo, error)
	Reviews(ctx context.Context, n int) ([]approve.Review, error)
	Approve(ctx context.Context, n int, body string) error
}

// Filer files and reads band-grants requests: approve.Filer.
type Filer interface {
	File(ctx context.Context, credential string, ttl time.Duration, reason string) (string, error)
	State(ctx context.Context, id string) (approve.RequestState, error)
}

// Pass is one round's input.
type Pass struct {
	Watch  model.OrgApproveWatch
	State  *model.OrgApproveState
	Grants func() ([]approve.Grant, error)
	Now    time.Time
	DryRun bool
}

// Out is one round's output.
type Out struct {
	Q     map[string]map[string]any // q id -> fields to set (asker fields only)
	Pings []plugin.Ping
	Log   []string
}

var (
	loginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	// QIDRe is the only q id shape this source writes.
	QIDRe  = regexp.MustCompile(`^oa-[0-9]{1,8}-[0-9a-f]{7}$`)
	sensRe = regexp.MustCompile(`(?i)(^\.github/|(^|/)ci/|pipeline|azure-pipelines|CODEOWNERS|secret|credential|(^|/)auth|policy|\.pfx$|\.pem$|keyvault)`)
	bots   = map[string]bool{"Ilya-Grebnov": true, "lukas": true, "automaton": true}
)

// ValidLogin reports whether s can be a GHE login in a search query.
func ValidLogin(s string) bool { return loginRe.MatchString(s) }

// QID is the q id of an ask.
func QID(pr int, head string) string { return fmt.Sprintf("oa-%d-%s", pr, head7(head)) }

func head7(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// Run is one round.
func Run(ctx context.Context, rd Reader, wr Writer, fl Filer, p Pass) (Out, error) {
	out := Out{Q: map[string]map[string]any{}}
	logf := func(f string, a ...any) { out.Log = append(out.Log, fmt.Sprintf(f, a...)) }
	st := p.State
	if st.PRs == nil {
		st.PRs = map[string]*model.OrgPR{}
	}
	if st.Asks == nil {
		st.Asks = map[string]*model.OrgAsk{}
	}
	open, err := lean(ctx, rd, p.Watch.Logins)
	if err != nil {
		return out, err
	}
	// Refresh the scored set: drop what closed, reset what moved.
	seen := map[string]bool{}
	for _, l := range open {
		k := strconv.Itoa(l.Number)
		seen[k] = true
		pr := st.PRs[k]
		if pr == nil || pr.Head != l.HeadRefOid {
			pr = &model.OrgPR{Number: l.Number}
			st.PRs[k] = pr
		}
		pr.Author, pr.Title, pr.Head = l.Author.Login, trunc(l.Title, 80), l.HeadRefOid
		if cf := cheapFails(l, p.Now); len(cf) > 0 {
			pr.Fails, pr.DeepAt, pr.Score, pr.Extra, pr.Sens, pr.Reasons = cf, "", 0, nil, nil, nil
		} else if pr.DeepAt == "" {
			pr.Fails = nil
		}
	}
	for k := range st.PRs {
		if !seen[k] {
			delete(st.PRs, k)
		}
	}
	// Deep-read the cheap-filter passers that are due.
	var due []*model.OrgPR
	for _, pr := range st.PRs {
		if pr.DeepAt == "" && len(pr.Fails) > 0 {
			continue // the cheap filters failed
		}
		if t, err := time.Parse(time.RFC3339, pr.DeepAt); err == nil && p.Now.Sub(t) < DeepEvery {
			continue
		}
		due = append(due, pr)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].DeepAt != due[j].DeepAt {
			return due[i].DeepAt < due[j].DeepAt // never read ("") first, then oldest
		}
		return due[i].Number < due[j].Number
	})
	if len(due) > DeepPerRound {
		logf("%d PR(s) due a deep read wait for later rounds", len(due)-DeepPerRound)
		due = due[:DeepPerRound]
	}
	if len(due) > 0 {
		nums := make([]int, len(due))
		for i, pr := range due {
			nums[i] = pr.Number
		}
		deep, err := deepRead(ctx, rd, nums)
		if err != nil {
			return out, err
		}
		for _, pr := range due {
			d := deep[pr.Number]
			if d == nil {
				logf("#%d: deep read returned nothing (left for the next round)", pr.Number)
				continue
			}
			if d.HeadRefOid != pr.Head {
				logf("#%d: head moved during the round (left for the next round)", pr.Number)
				continue
			}
			score(pr, d, p.Watch.KnownBreaks, p.Now)
			pr.DeepAt = p.Now.UTC().Format(time.RFC3339)
		}
	}
	if err := asks(ctx, wr, fl, p, &out, logf); err != nil {
		return out, err
	}
	nc := 0
	for _, pr := range st.PRs {
		if pr.Candidate() {
			nc++
		}
	}
	logf("%d open PR(s) in the org, %d deep-read this round, %d candidate(s), %d ask(s) open; %s",
		len(st.PRs), len(due), nc, openAsks(st), rd.Spent())
	return out, nil
}

func openAsks(st *model.OrgApproveState) int {
	n := 0
	for _, a := range st.Asks {
		if a.State == "open" {
			n++
		}
	}
	return n
}

// asks files new asks and decides the open ones.
func asks(ctx context.Context, wr Writer, fl Filer, p Pass, out *Out, logf func(string, ...any)) error {
	st, w := p.State, p.Watch
	now := p.Now.UTC()
	at := now.Format(time.RFC3339)
	var grants []approve.Grant
	grantsRead := false
	ids := make([]string, 0, len(st.Asks))
	for id := range st.Asks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := st.Asks[id]
		if a.State != "open" {
			if t, err := time.Parse(time.RFC3339, a.FiledAt); err == nil && now.Sub(t) > KeepDone {
				delete(st.Asks, id)
			}
			continue
		}
		pr := st.PRs[strconv.Itoa(a.PR)]
		switch {
		case pr == nil:
			closeAsk(out, st, id, "withdrawn", "the PR is no longer open in the org's list")
			continue
		case pr.Head != a.Head:
			closeAsk(out, st, id, "withdrawn", "head moved to "+head7(pr.Head)+": asked again at the new head if still ready")
			continue
		case pr.DeepAt != "" && !pr.Candidate():
			closeAsk(out, st, id, "withdrawn", "no longer ready: "+strings.Join(append(append([]string{}, pr.Fails...), pr.Extra...), ", "))
			continue
		}
		rs, err := fl.State(ctx, a.RequestID)
		if err != nil {
			logf("%s: request %s unreadable: %v (next round)", id, a.RequestID, err)
			continue
		}
		switch rs.State {
		case "pending":
			if t, err := time.Parse(time.RFC3339, a.FiledAt); err == nil && now.Sub(t) > PendingLapse+time.Hour {
				closeAsk(out, st, id, "withdrawn", "request "+a.RequestID+" lapsed unanswered")
			}
			continue
		case "denied":
			closeAsk(out, st, id, "done", "not approved: request "+a.RequestID+" denied on /grants/")
			continue
		case "approved":
		default:
			closeAsk(out, st, id, "withdrawn", "request "+a.RequestID+" is "+rs.State)
			continue
		}
		if !grantsRead {
			if grants, err = p.Grants(); err != nil {
				logf("grants unreadable: %v (approved asks wait)", err)
				return nil
			}
			grantsRead = true
		}
		if why := tapGrant(grants, a.RequestID, w, now.Unix()); why != "" {
			if strings.HasPrefix(why, "expired") {
				closeAsk(out, st, id, "withdrawn", "the approval of request "+a.RequestID+" expired before the post")
			} else {
				logf("%s: request %s approved, but %s: nothing posted", id, a.RequestID, why)
			}
			continue
		}
		res, why, err := post(ctx, wr, a, p.DryRun)
		if err != nil {
			logf("%s: %v (next round)", id, err)
			continue
		}
		if why != "" {
			closeAsk(out, st, id, "withdrawn", "approved on /grants/ but not posted: "+why)
			continue
		}
		if p.DryRun {
			logf("dry run: %s would be approved", id)
			continue
		}
		a.ReviewID = res
		closeAsk(out, st, id, "done", fmt.Sprintf("approved %s#%d at %s, review %d (%s)", Repo, a.PR, head7(a.Head), res, at))
		st.Asks[id].State = "approved"
		if w.PingTo != "" {
			out.Pings = append(out.Pings, plugin.Ping{To: w.PingTo, Text: fmt.Sprintf(
				"org-approve: %s#%d approved at %s (review %d, request %s)", Repo, a.PR, head7(a.Head), res, a.RequestID)})
		}
	}
	// New asks: candidates at a head never asked, within band-grants' cap.
	var cands []*model.OrgPR
	for _, pr := range st.PRs {
		if pr.Candidate() && st.Asks[QID(pr.Number, pr.Head)] == nil {
			cands = append(cands, pr)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Number < cands[j].Number })
	for _, pr := range cands {
		if openAsks(st) >= MaxOpenAsks {
			logf("%d candidate(s) wait: %d asks open (band-grants max pending)", len(cands), MaxOpenAsks)
			break
		}
		id := QID(pr.Number, pr.Head)
		if p.DryRun {
			logf("dry run: would ask %s (%s#%d by %s)", id, Repo, pr.Number, pr.Author)
			continue
		}
		reason := fmt.Sprintf("APPROVE %s#%d at %s", Repo, pr.Number, head7(pr.Head))
		rid, err := fl.File(ctx, w.Grant, AskTTL, reason)
		if err != nil {
			logf("%s: request not filed: %v (next round)", id, err)
			continue
		}
		st.Asks[id] = &model.OrgAsk{PR: pr.Number, Head: pr.Head, RequestID: rid, FiledAt: at, State: "open"}
		ctxt := fmt.Sprintf("https://%s/%s/pull/%d by %s: %s. Score 6/6: %s.", approve.Host, Repo, pr.Number, pr.Author, pr.Title, strings.Join(pr.Reasons, "; "))
		if len(pr.Sens) > 0 {
			ctxt += " Sensitive paths (never auto-approved): " + strings.Join(pr.Sens, ", ") + "."
		}
		ctxt += fmt.Sprintf(" To approve: Approve request %s (%s) on %s within 24 h; ebac posts the review at head %s only. An answer here does not approve.", rid, reason, GrantsPage, head7(pr.Head))
		out.Q[id] = map[string]any{
			"ask":     fmt.Sprintf("Approve %s#%d (%s) by %s?", Repo, pr.Number, trunc(pr.Title, 60), pr.Author),
			"context": ctxt,
			"since":   at,
			"default": "nothing: no approval without your Approve on /grants/",
			"order":   pr.Number,
			"status":  "open",
		}
		if w.PingTo != "" {
			out.Pings = append(out.Pings, plugin.Ping{To: w.PingTo, Text: fmt.Sprintf(
				"org-approve: asked %s#%d at %s (q %s, /grants/ request %s)", Repo, pr.Number, head7(pr.Head), id, rid)})
		}
	}
	return nil
}

// closeAsk ends an ask: its q gets status and resolved; its state follows.
func closeAsk(out *Out, st *model.OrgApproveState, id, status, resolved string) {
	out.Q[id] = map[string]any{"status": status, "resolved": resolved}
	if a := st.Asks[id]; a != nil {
		a.State = status
		if status == "done" {
			a.State = "decided"
		}
	}
}

// tapGrant is "" when the grants file holds the row of exactly this request,
// for w's principal and credential, approved by w.Approver, live at now.
func tapGrant(gs []approve.Grant, rid string, w model.OrgApproveWatch, now int64) string {
	for _, g := range gs {
		if g.ID != rid {
			continue
		}
		switch {
		case g.Principal != w.Principal || g.Credential != w.Grant:
			return fmt.Sprintf("its grant row is %s for %s, not %s for %s", g.Credential, g.Principal, w.Grant, w.Principal)
		case g.ApprovedBy != w.Approver:
			return fmt.Sprintf("approved by %q, not %q", g.ApprovedBy, w.Approver)
		case g.ExpiresAt <= now:
			return "expired at " + strconv.FormatInt(g.ExpiresAt, 10)
		}
		return ""
	}
	return "expired: no live grant row " + rid
}

// post re-checks the PR at the post (open, not draft, head unchanged, no
// human change request standing, not already approved by Jack) and approves.
// why is a final refusal; err a transient failure.
func post(ctx context.Context, wr Writer, a *model.OrgAsk, dry bool) (int64, string, error) {
	info, err := wr.PR(ctx, a.PR)
	if err != nil {
		return 0, "", fmt.Errorf("read PR: %w", err)
	}
	switch {
	case info.State != "open":
		return 0, "the PR is " + info.State, nil
	case info.Draft:
		return 0, "the PR is a draft", nil
	case info.Head.SHA != a.Head:
		return 0, "head moved to " + head7(info.Head.SHA), nil
	}
	rvs, err := wr.Reviews(ctx, a.PR)
	if err != nil {
		return 0, "", fmt.Errorf("read reviews: %w", err)
	}
	latest := map[string]string{}
	for _, r := range rvs {
		if r.User.Type == "Bot" || isBotLogin(r.User.Login) {
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[r.User.Login] = r.State
		}
		if r.User.Login == Self && r.State == "APPROVED" && strings.TrimSpace(r.Body) == Body {
			return r.ID, "", nil // posted in an earlier round that never recorded it
		}
	}
	for l, s := range latest {
		if s == "CHANGES_REQUESTED" {
			return 0, "human change request by " + l, nil
		}
	}
	if latest[Self] == "APPROVED" {
		return 0, "Jack already approved it", nil
	}
	if dry {
		return 0, "", nil
	}
	perr := wr.Approve(ctx, a.PR, Body)
	back, err := wr.Reviews(ctx, a.PR)
	if err != nil {
		return 0, "", fmt.Errorf("read back: %w", err)
	}
	for _, r := range back {
		if r.State == "APPROVED" && strings.TrimSpace(r.Body) == Body {
			return r.ID, "", nil
		}
	}
	if perr != nil {
		return 0, "", fmt.Errorf("post: %w", perr)
	}
	return 0, "approve sent, but no APPROVED review with its body read back", nil
}

func isBotLogin(l string) bool { return bots[l] || strings.Contains(strings.ToLower(l), "bot") }

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
