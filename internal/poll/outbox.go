package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// validPostID keeps an id usable as one form path segment.
func ValidPostID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("post id %q: 1-64 characters", id)
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("post id %q: use only [A-Za-z0-9_-]", id)
		}
	}
	return nil
}

func NewPostID(now time.Time) string {
	return "p" + strconv.FormatInt(now.UnixNano(), 36)
}

// authorize says why a post may not go out, or "" if it may.
func Authorize(w *model.Critic, snap *model.Snapshot, me string, p *model.Post) string {
	if !w.Write && w.Mode != model.ModeReviewer {
		return "this critic is read-only: write comes from ebac's trusted config (authority.write), not from its form"
	}
	if w.Mode == model.ModeKeel {
		return authorizeKeel(w, p)
	}
	if w.Approve && (w.DiscoverRepo != "" || w.DiscoverAuthor != "") {
		// discover_* are form-held: on an approve critic they would widen the
		// PRs it may post on and approve.
		return "an approve critic may not discover PRs (discover_repo/discover_author on its form): refused; name its PRs and approve_prs in ebac's trusted config"
	}
	if p.React != "" {
		if p.ReplyTo == "" {
			return "react needs reply_to: the thread whose opening comment gets the reaction"
		}
		if _, ok := forge.Reactions[p.React]; !ok {
			return fmt.Sprintf("reaction %q: use one of +1 -1 laugh hooray confused heart rocket eyes", p.React)
		}
	}
	if p.Resolve {
		if p.ReplyTo == "" {
			return "resolve needs reply_to: the thread to resolve"
		}
	}
	if !p.Resolve && p.React == "" && !p.HasBody() {
		return "empty body"
	}
	if why := disclose.Disclosure(p.Body); why != "" {
		return why
	}
	var pr *model.PRState
	if snap != nil {
		pr = snap.PRs[model.PRKey(p.PR)]
	}
	if pr == nil {
		return fmt.Sprintf("%q is not a pull request of this critic", p.PR)
	}
	if len(pr.Pending) > 0 && (p.Event != "" || p.Path != "" || (p.ReplyTo != "" && p.HasBody())) {
		r := pr.Pending[0]
		return fmt.Sprintf("pending review %d by %s on %s (opened %s, %d comments only its author can see) would swallow a reply and refuses new reviews: "+
			"the operator must submit or delete it; a top-level comment (no --reply-to, --path or --event), --react and --resolve still work",
			r.DatabaseID, r.Author, p.PR, r.CreatedAt, r.Comments)
	}
	switch p.Event {
	case "", "COMMENT":
	case "APPROVE":
		if !w.Approve || w.Mode != model.ModeReviewer {
			return "approval is not granted on this critic"
		}
		if strings.EqualFold(pr.Author, me) {
			return "a seat may not approve its own operator's pull request"
		}
		if ref := model.ApproveRef(pr.Owner+"/"+pr.Repo, pr.Number, pr.HeadSHA); pr.HeadSHA == "" || !slices.Contains(w.ApprovePRs, ref) {
			return fmt.Sprintf("approval of %s is not in ebac's trusted approve_prs (owner/name#n@<head sha>; the head may have moved)", ref)
		}
	default:
		return fmt.Sprintf("event %q is never allowed (only COMMENT or APPROVE)", p.Event)
	}
	if p.ReplyTo != "" {
		if _, ok := pr.Threads[p.ReplyTo]; !ok {
			return fmt.Sprintf("thread %s is not on %s", p.ReplyTo, p.PR)
		}
	}
	if (p.Path == "") != (p.Line == 0) {
		return "an inline comment needs both path and line"
	}
	if p.ReplyTo != "" && p.Path != "" {
		return "give reply_to or path/line, not both"
	}
	return ""
}

// FlushOutbox posts what the seat wrote since the watermark and returns the
// receipts for the round's one patch, and moves the watermark in w. On any
// read failure it moves nothing. The core authorizes every post; only
// authorized posts reach the plugin (plugin.OpPost), which takes them as far
// as they go upstream and returns their receipt fields.
func (p *Poller) FlushOutbox(ctx context.Context, w *model.Critic, snap *model.Snapshot) form.Receipts {
	acks := form.Receipts{}
	c, err := p.Fig.Node(ctx, w.FormID)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	since, err := c.FormSince(ctx, w.Runtime.OutboxWatermark)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	cur, err := p.Fig.Form(ctx, w.FormID)
	if err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unreadable: %v\n", w.Name, err)
		return acks
	}
	var posts map[string]*model.Post
	if err := json.Unmarshal(model.MustJSON(cur["post"]), &posts); err != nil {
		fmt.Fprintf(p.Out, "%s: outbox unparseable: %v\n", w.Name, err)
		return acks
	}
	ids := map[string]bool{}
	if since.Delta {
		for _, vp := range since.Patches {
			for _, e := range vp.Patch.Entries() {
				if rest, ok := strings.CutPrefix(e.Key, "post."); ok {
					ids[strings.SplitN(rest, ".", 2)[0]] = true
				} else if e.Key == "post" {
					for id := range posts {
						ids[id] = true
					}
				}
			}
		}
	} else {
		for id := range posts {
			ids[id] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	todo := map[string]*model.Post{}
	hadError := map[string]bool{}
	alarms := map[string]json.RawMessage{}
	var wakes []plugin.Ping
	var order []string
	for _, id := range sorted {
		post := posts[id]
		if post == nil || post.Terminal() {
			continue
		}
		hadError[id] = post.Error != ""
		if err := ValidPostID(id); err != nil {
			post.Refused = err.Error()
		} else if why := Authorize(w, snap, p.Me, post); why != "" {
			post.Refused = why
		} else if why, alarm, wake := approveGate(w, snap, id, post, time.Now().UTC()); why != "" {
			post.Refused, alarms[id] = why, alarm
			if wake != nil {
				wakes = append(wakes, *wake)
			} else {
				fmt.Fprintf(p.Out, "%s: approval %s blocked, no trusted maintainer to wake\n", w.Name, id)
			}
		} else {
			todo[id] = post
		}
		order = append(order, id)
	}
	wakes = append(wakes, p.postJudged(ctx, w, snap, order, todo, alarms, time.Now().UTC())...)
	for _, id := range order {
		post := posts[id]
		r := map[string]json.RawMessage{}
		if post.Sent != nil {
			r["sent"] = model.MustJSON(post.Sent)
		}
		if post.Resolved != "" {
			r["resolved"] = model.MustJSON(post.Resolved)
		}
		if post.Reacted != "" {
			r["reacted"] = model.MustJSON(post.Reacted)
		}
		if post.Refused != "" {
			r["refused"] = model.MustJSON(post.Refused)
		}
		if a := alarms[id]; a != nil {
			r["alarm"] = a
		}
		if post.Error != "" || hadError[id] {
			r["error"] = model.MustJSON(post.Error)
		}
		if post.Attempts > 0 {
			r["attempts"] = model.MustJSON(post.Attempts)
		}
		if post.Terminal() {
			r["done_at"] = model.MustJSON(time.Now().UTC().Format(time.RFC3339))
		}
		acks[id] = r
		switch {
		case post.Refused != "":
			fmt.Fprintf(p.Out, "%s: refused %s: %s\n", w.Name, id, post.Refused)
		case post.Error != "":
			fmt.Fprintf(p.Out, "%s: post %s failed (attempt %d): %s\n", w.Name, id, post.Attempts, post.Error)
		default:
			url := "(no reply)"
			if post.Sent != nil {
				url = post.Sent.URL
			}
			res := ""
			if post.Resolved != "" {
				res = ", thread resolved"
			}
			fmt.Fprintf(p.Out, "%s: posted %s -> %s%s\n", w.Name, id, url, res)
		}
	}
	w.Runtime.OutboxWatermark = since.Version
	// One wake per blocked approval: a refused post is terminal, never
	// judged again.
	p.sendPings(ctx, w, wakes)
	return acks
}

// approveGate applies the blocker rule (model.ApproveBlockers) to an APPROVE
// Authorize allowed, on this round's snapshot (bodies read fresh): it says
// why the approval may not go out, the alarm for the post's receipt
// (post.<id>.alarm) and the one wake of the trusted maintainer (nil when the
// critic has none). Fail closed: no automated marker, no approval.
func approveGate(w *model.Critic, snap *model.Snapshot, id string, post *model.Post, now time.Time) (string, json.RawMessage, *plugin.Ping) {
	if post.Event != "APPROVE" || snap == nil {
		return "", nil, nil
	}
	pr := snap.PRs[model.PRKey(post.PR)]
	if pr == nil {
		return "", nil, nil // Authorize refused it already
	}
	bs := model.ApproveBlockers(pr, w.AutomatedMarkers)
	if len(bs) == 0 {
		return "", nil, nil
	}
	return approveAlarm(w, id, post, pr.HeadSHA, model.ApproveBlocked+strings.Join(bs, "; "), bs[0], len(bs), now)
}

// postJudged sends the authorized posts (p.post) and returns the wakes for
// the approvals the plugin's fresh read blocked (pluginBlocked).
func (p *Poller) postJudged(ctx context.Context, w *model.Critic, snap *model.Snapshot, order []string, todo map[string]*model.Post, alarms map[string]json.RawMessage, now time.Time) []plugin.Ping {
	p.post(ctx, w, snap, todo)
	return pluginBlocked(w, snap, order, todo, alarms, now)
}

// pluginBlocked turns each approval the plugin's fresh read blocked at send
// time (post.Refused carries model.ApproveBlocked) into its
// receipt alarm (into alarms) and its one wake.
func pluginBlocked(w *model.Critic, snap *model.Snapshot, order []string, todo map[string]*model.Post, alarms map[string]json.RawMessage, now time.Time) []plugin.Ping {
	var wakes []plugin.Ping
	for _, id := range order {
		post := todo[id]
		if post == nil || post.Event != "APPROVE" || !strings.HasPrefix(post.Refused, model.ApproveBlocked) || alarms[id] != nil {
			continue
		}
		bs := strings.Split(strings.TrimPrefix(post.Refused, model.ApproveBlocked), "; ")
		head := ""
		if pr := snap.PRs[model.PRKey(post.PR)]; pr != nil {
			head = pr.HeadSHA
		}
		var wake *plugin.Ping
		_, alarms[id], wake = approveAlarm(w, id, post, head, post.Refused, bs[0], len(bs), now)
		if wake != nil {
			wakes = append(wakes, *wake)
		}
	}
	return wakes
}

// approveAlarm is a blocked approval's receipt alarm and its one wake of the
// trusted maintainer (nil when the critic has none): head is the PR head it
// was judged at, first the first blocker, n how many.
func approveAlarm(w *model.Critic, id string, post *model.Post, head, why, first string, n int, now time.Time) (string, json.RawMessage, *plugin.Ping) {
	alarm := model.MustJSON(map[string]string{"kind": "approve-blocked", "at": now.Format(time.RFC3339), "head": head, "detail": why})
	if w.Maintainer == "" {
		return why, alarm, nil
	}
	sha := head
	if len(sha) > 12 {
		sha = sha[:12]
	}
	text := strings.Join(strings.Fields(fmt.Sprintf("ebac: approval blocked, %s post %s, %s@%s: %s (%d blocker(s); receipt post.%s.alarm)",
		w.Name, id, post.PR, sha, first, n, id)), " ")
	if len(text) > 400 {
		text = text[:397] + "..."
	}
	return why, alarm, &plugin.Ping{To: w.Maintainer, Text: text}
}

// post hands authorized posts to the source's plugin and merges back what it
// did. A plugin failure is every post's error for this round: each is retried
// on the next.
func (p *Poller) post(ctx context.Context, w *model.Critic, snap *model.Snapshot, todo map[string]*model.Post) {
	if w.Mode == model.ModeKeel {
		// The enforcing point: the writer must still hold the role form
		// recorded when the post was written, checked right before sending.
		p.recheckKeelRoles(ctx, w, todo)
	}
	if len(todo) == 0 {
		return
	}
	rep, err := p.call(ctx, w, plugin.Request{Op: plugin.OpPost, Snapshot: snap, Posts: todo})
	if err != nil {
		for _, post := range todo {
			post.Error = err.Error()
		}
		return
	}
	for _, l := range rep.Log {
		fmt.Fprintln(p.Out, l)
	}
	for id, post := range todo {
		if got := rep.Posts[id]; got != nil {
			*post = *got
		} else {
			post.Error = "the plugin returned no result for this post"
		}
	}
}

// keelRoleForm is a figaro form id: 32 hex, or the 8-hex short id a few
// anchored role forms carry.
var keelRoleForm = regexp.MustCompile(`^@[0-9a-f]{8}([0-9a-f]{24})?$`)

// KeelBodyMax is keel-web's comment limit (16384 chars) less room for the
// door's stamp and the post marker.
const KeelBodyMax = 16000

// authorizeKeel is Authorize for a keel critic: a reply (reply_to), a new
// thread on a file line (path + line, optional rev), and resolve (reply_to).
// keel has no reviews, events or reactions. The writing seat must be named:
// keel's door stamps every comment with it.
func authorizeKeel(w *model.Critic, p *model.Post) string {
	if w.Keel == nil {
		return "keel critic without a repo"
	}
	if p.PR != w.Keel.Repo {
		return fmt.Sprintf("%q is not this critic's keel repo (%s)", p.PR, w.Keel.Repo)
	}
	if p.Event != "" {
		return "keel has no reviews: event is not allowed"
	}
	if p.React != "" {
		return "keel has no reactions"
	}
	if why := model.KeelWriter(p.ByAria, p.ByRole); why != "" {
		return why
	}
	if !keelRoleForm.MatchString(p.ByRoleForm) {
		return "by_role_form: a keel post is written by `ebac post`, which checks the role and records its form"
	}
	if p.Resolve && p.ReplyTo == "" {
		return "resolve needs reply_to: the thread to resolve"
	}
	if !p.Resolve && !p.HasBody() {
		return "empty body"
	}
	if len([]rune(p.Body)) > KeelBodyMax {
		return fmt.Sprintf("body is %d chars: keel takes at most %d", len([]rune(p.Body)), KeelBodyMax)
	}
	// keel is internal, so a keel thread may name our tools, roles and nodes: the internal-term scrub guards only
	// text bound for the github plugin's forges (GHE, github.com). It is skipped only when the post really goes to
	// keel (source keel-threads): a critic whose mode says keel but whose source is a forge stays scrubbed.
	if w.SourceName() != "keel-threads" {
		if why := disclose.Disclosure(p.Body); why != "" {
			return why
		}
	}
	if (p.Path == "") != (p.Line == 0) {
		return "a new thread needs both path and line"
	}
	if p.Line < 0 {
		return "line must be positive"
	}
	if p.ReplyTo != "" && p.Path != "" {
		return "give reply_to or path/line, not both"
	}
	if p.ReplyTo == "" && p.Path == "" {
		return "a keel post needs reply_to (a reply) or path + line (a new thread)"
	}
	if p.Rev != "" && p.Path == "" {
		return "rev anchors a new thread: it needs path and line"
	}
	if p.Path != "" && !model.KeelPathMatches(p.Path, w.Keel.Paths) {
		return fmt.Sprintf("path %s is outside this critic's paths %v", p.Path, w.Keel.Paths)
	}
	return ""
}

// recheckKeelRoles re-checks, right before sending, that each keel post's
// writer still holds the role form recorded when the post was written. A
// read failure holds the post for the next round (never sent unchecked); a
// role no longer held, or resolving to another form, refuses it.
func (p *Poller) recheckKeelRoles(ctx context.Context, w *model.Critic, todo map[string]*model.Post) {
	check := p.PostCheck
	if p.RoleCheck != nil {
		check = p.RoleCheck
	}
	for id, post := range todo {
		form, why, err := check(ctx, post.ByAria, post.ByRole)
		switch {
		case err != nil:
			post.Error = "role check: " + err.Error()
			delete(todo, id)
		case why != "" && form == post.ByRoleForm:
			post.Refused = "the holder of " + post.ByRole + " changed since the post was written (succession): " + why
			delete(todo, id)
		case why != "":
			post.Refused = why
			delete(todo, id)
		case form != post.ByRoleForm:
			post.Refused = fmt.Sprintf("role %s moved to another role form (%s, was %s when the post was written)", post.ByRole, form, post.ByRoleForm)
			delete(todo, id)
		}
	}
}
