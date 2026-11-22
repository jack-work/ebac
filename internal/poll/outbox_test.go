package poll

import (
	"context"
	"encoding/json"
	"github.com/gluck/ebac/internal/livetest"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func outboxCritic() (*model.Critic, *model.Snapshot) {
	pr := &model.PRState{
		Key: model.MakePRKey("acme", "widget", 1), Owner: "acme", Repo: "widget", Number: 1, URL: "https://x/pull/1",
		State: "OPEN", Author: "friend", HeadSHA: "abc123",
		Threads:  map[string]*model.Thread{"PRRT_1": {ID: "PRRT_1", Path: "a.go", Line: 3}},
		Reviews:  map[string]*model.Review{},
		Comments: map[string]*model.Comment{},
	}
	w := &model.Critic{Name: "rw", Mode: model.ModeReview, Write: true, FormID: "@f"}
	return w, &model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}}
}

func TestAuthorizeKeepsPostsInsideTheGrant(t *testing.T) {
	w, snap := outboxCritic()
	ro := *w
	ro.Write = false
	rev := model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true, ApprovePRs: []string{"acme/widget#1@abc123"}}
	pw := w
	_, pendingSnap := outboxCritic()
	pendingSnap.PRs["acme/widget#1"].Pending = []model.PendingReview{{ID: "PRR_p", DatabaseID: 213120916, Author: "jack", Comments: 344}}
	revNoApprove := model.Critic{Name: "rev", Mode: model.ModeReviewer}
	for _, tc := range []struct {
		name string
		w    *model.Critic
		me   string
		post model.Post
		want string // substring of the refusal; "" means allowed
	}{
		{"pr comment", w, "jack", model.Post{PR: "acme/widget#1", Body: "x"}, ""},
		{"thread reply", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_1"}, ""},
		{"inline", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", Path: "a.go", Line: 3}, ""},
		{"read-only critic", &ro, "jack", model.Post{PR: "acme/widget#1", Body: "x"}, "read-only"},
		{"foreign pr", w, "jack", model.Post{PR: "acme/other#9", Body: "x"}, "not a pull request of this critic"},
		{"empty", w, "jack", model.Post{PR: "acme/widget#1", Body: "  "}, "empty body"},
		{"unknown thread", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_9"}, "not on"},
		{"path without line", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", Path: "a.go"}, "both path and line"},
		{"request changes", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", Event: "REQUEST_CHANGES"}, "never allowed"},
		{"approve without grant", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "not granted"},
		{"reviewer approves", &rev, "jack", model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, ""},
		{"reviewer without approve", &revNoApprove, "jack", model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "not granted"},
		{"approve own operator's pr", &rev, "friend", model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "own operator"},
		{"reply and resolve", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_1", Resolve: true}, ""},
		{"resolve alone", w, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true}, ""},
		{"resolve without a thread", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", Resolve: true}, "needs reply_to"},
		{"reviewer resolves", &rev, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true}, ""},
		{"read-only resolves", &ro, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true}, "read-only"},
		{"react alone", w, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "+1"}, ""},
		{"reviewer reacts", &rev, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "+1"}, ""},
		{"react without a thread", w, "jack", model.Post{PR: "acme/widget#1", Body: "x", React: "+1"}, "needs reply_to"},
		{"unknown reaction", w, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "thumbsup"}, "use one of"},
		{"pending: reply", pw, "jack", model.Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_1"}, "pending review 213120916"},
		{"pending: inline", pw, "jack", model.Post{PR: "acme/widget#1", Body: "x", Path: "a.go", Line: 3}, "pending review"},
		{"pending: review", pw, "jack", model.Post{PR: "acme/widget#1", Body: "x", Event: "COMMENT"}, "pending review"},
		{"pending: top-level comment", pw, "jack", model.Post{PR: "acme/widget#1", Body: "x"}, ""},
		{"pending: react and resolve", pw, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "+1", Resolve: true}, ""},
	} {
		s := snap
		if strings.HasPrefix(tc.name, "pending:") {
			s = pendingSnap
		}
		got := Authorize(tc.w, s, tc.me, &tc.post)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusal %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------- live: the form is the state ----------

func liveCritic(t *testing.T, f *figaro.Figaro, write bool) (*form.Store, *model.Critic, *model.Snapshot) {
	t.Helper()
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	st.Me = "jack"
	id, err := f.FormNew(ctx, "ebac-livetest-state")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, id)
	w, snap := outboxCritic()
	w.Name = "livetest-" + strings.TrimPrefix(id, "@")[:8]
	w.Write = write
	w.FormID = id
	w.Runtime.Round = 7
	snap.Critic, snap.Round, snap.TakenAt, snap.Complete = w.Name, 7, time.Now().UTC().Truncate(time.Second), true
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments = []*model.Comment{{ID: "C1", Author: "friend", Body: "first", CreatedAt: "2026-10-20T01:00:00Z"}}
	if err := st.SaveRecordingForTest(ctx, w, snap, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	return st, w, snap
}

func TestLiveSaveIsOnePatchThatKeepsSeatKeys(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)

	if err := f.SetJSON(ctx, w.FormID, "ack_note", "seat wrote this"); err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadForm(ctx, w.FormID)
	if err != nil {
		t.Fatal(err)
	}
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments = append(snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments,
		&model.Comment{ID: "C2", Author: "amy", Body: "second", CreatedAt: "2026-10-20T02:00:00Z"})
	w.Runtime.Round = 8
	if err := st.SaveRecordingForTest(ctx, w, snap, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := f.Node(ctx, w.FormID)
	since, err := c.FormSince(ctx, before.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !since.Delta || len(since.Patches) != 1 {
		t.Fatalf("a save wrote %d patches (delta=%v); want exactly one", len(since.Patches), since.Delta)
	}
	var seen []string
	for _, e := range since.Patches[0].Patch.Entries() {
		if !strings.HasPrefix(e.Key, "system.") {
			seen = append(seen, e.Key)
		}
	}
	// The thread detail is on disk (prs_local.go): the form sees the new
	// comment as its PR head's comment count.
	for _, k := range seen {
		if k != "prs.acme/widget#1.n.comments" {
			t.Errorf("seat-visible change outside the new comment's count: %s", k)
		}
	}
	if len(seen) == 0 {
		t.Fatal("positive control: the new comment is not in the patch")
	}

	st2, _ := form.NewStore(figaro.NewFigaro(""))
	got, err := st2.LoadCritic(ctx, w.Name)
	if err != nil {
		t.Fatalf("discover by name: %v", err)
	}
	if got.FormID != w.FormID || got.Runtime.Round != 8 || !got.Write {
		t.Fatalf("critic round-trip: %+v", got)
	}
	gs, err := st2.LoadSnapshot(ctx, w.Name)
	if err != nil || gs == nil {
		t.Fatalf("snapshot: %v", err)
	}
	if cs := gs.PRs["acme/widget#1"].Threads["PRRT_1"].Comments; len(cs) != 2 || cs[1].ID != "C2" || cs[1].Author != "amy" || cs[1].Body != "" {
		t.Fatalf("thread round-trip: %+v", cs)
	}
	form, _ := f.Form(ctx, w.FormID)
	if form["ack_note"] != "seat wrote this" {
		t.Errorf("seat key overwritten: %#v", form["ack_note"])
	}
}

func writePost(t *testing.T, st *form.Store, w *model.Critic, id string, p model.Post) {
	t.Helper()
	err := st.Commit(context.Background(), w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		posts := model.ObjectOf(cur["post"])
		posts[id] = model.MustJSON(p)
		return map[string]json.RawMessage{"post": model.MustJSON(posts)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readPost(t *testing.T, f *figaro.Figaro, w *model.Critic, id string) model.Post {
	t.Helper()
	form, err := f.Form(context.Background(), w.FormID)
	if err != nil {
		t.Fatal(err)
	}
	var posts map[string]model.Post
	_ = json.Unmarshal(model.MustJSON(form["post"]), &posts)
	return posts[id]
}

// An approval goes out only for a PR and head named in the trusted approve_prs. Control: the listed PR at
// the listed head is approved. RED if the approve_prs check goes (the three refusals pass) or compares the PR
// without its head (the moved-head row passes).
func TestApproveNeedsTheTrustedPRAtItsHead(t *testing.T) {
	_, snap := outboxCritic()
	approve := model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}
	for _, tc := range []struct {
		name string
		list []string
		want string // the exact refusal; "" means allowed
	}{
		{"listed at its head", []string{"acme/widget#1@abc123"}, ""},
		{"no approve_prs", nil, "approval of acme/widget#1@abc123 is not in ebac's trusted approve_prs (owner/name#n@<head sha>; the head may have moved)"},
		{"head moved since it was listed", []string{"acme/widget#1@0ld5ha"}, "approval of acme/widget#1@abc123 is not in ebac's trusted approve_prs (owner/name#n@<head sha>; the head may have moved)"},
		{"another PR listed", []string{"acme/widget#2@abc123"}, "approval of acme/widget#1@abc123 is not in ebac's trusted approve_prs (owner/name#n@<head sha>; the head may have moved)"},
	} {
		w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true, ApprovePRs: tc.list}
		p := approve
		if got := Authorize(w, snap, "jack", &p); got != tc.want {
			t.Errorf("%s: refusal %q, want %q", tc.name, got, tc.want)
		}
	}
}

// discover_repo / discover_author are form-held, so an approve critic that carries either posts
// nothing. Control: the same critic without discovery posts the same comment. RED if the discover clause goes, or
// loses either half (each row sets one).
func TestAnApproveCriticWithDiscoveryPostsNothing(t *testing.T) {
	_, snap := outboxCritic()
	const refused = "an approve critic may not discover PRs (discover_repo/discover_author on its form): refused; name its PRs and approve_prs in ebac's trusted config"
	for _, tc := range []struct {
		name, repo, author, want string
	}{
		{"no discovery", "", "", ""},
		{"discover_repo", "acme/widget", "", refused},
		{"discover_author", "", "friend", refused},
	} {
		w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true, ApprovePRs: []string{"acme/widget#1@abc123"},
			DiscoverRepo: tc.repo, DiscoverAuthor: tc.author}
		p := model.Post{PR: "acme/widget#1", Body: "x"}
		if got := Authorize(w, snap, "jack", &p); got != tc.want {
			t.Errorf("%s: refusal %q, want %q", tc.name, got, tc.want)
		}
	}
	plain := &model.Critic{Name: "rw", Mode: model.ModeReview, Write: true, DiscoverRepo: "acme/widget"}
	if got := Authorize(plain, snap, "jack", &model.Post{PR: "acme/widget#1", Body: "x"}); got != "" {
		t.Errorf("a write critic without approve keeps discovery: refusal %q", got)
	}
}

// The blocker rule at the head an approve_prs entry names: a standing
// REQUEST_CHANGES or an unresolved thread marked blocking stops the approval unless its body carries one of the
// critic's trusted automated markers; then the refusal carries an alarm for the receipt and ONE wake of the trusted
// maintainer. Markers here are synthesized (the real list lives only in the trusted config). Controls: a clean head
// approves; each configured marker exempts its review or thread. RED if the gate goes (blocked rows approve), the
// marker check goes (marker rows block) or matches partially (the partial row approves), or an empty list exempts.
func TestApproveGateBlocksUnlessAutomated(t *testing.T) {
	markers := []string{"[synthetic-bot: automated review]", "Synthetic tally:"}
	head := func(mut func(pr *model.PRState)) *model.Snapshot {
		_, snap := outboxCritic()
		pr := snap.PRs["acme/widget#1"]
		pr.Complete = true
		pr.Threads["PRRT_1"].Comments = []*model.Comment{{ID: "C1", Author: "friend", Body: "nit: rename this"}}
		if mut != nil {
			mut(pr)
		}
		return snap
	}
	cr := func(id, who, at, state, body string) func(pr *model.PRState) {
		return func(pr *model.PRState) {
			pr.Reviews[id] = &model.Review{ID: id, Author: who, State: state, SubmittedAt: at, Body: body}
		}
	}
	both := func(fs ...func(pr *model.PRState)) func(pr *model.PRState) {
		return func(pr *model.PRState) {
			for _, f := range fs {
				f(pr)
			}
		}
	}
	blockingThread := func(body string, resolved bool) func(pr *model.PRState) {
		return func(pr *model.PRState) {
			pr.Threads["PRRT_2"] = &model.Thread{ID: "PRRT_2", Path: "b.go", IsResolved: resolved,
				Comments: []*model.Comment{{ID: "C2", Author: "reviewer-b", Body: body}}}
		}
	}
	for _, tc := range []struct {
		name    string
		markers []string
		mut     func(pr *model.PRState)
		want    string // the exact refusal; "" means it goes out
	}{
		{"clean head", markers, nil, ""},
		{"a person's REQUEST_CHANGES", markers, cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "Please split this."),
			model.ApproveBlocked + "a REQUEST_CHANGES by reviewer-a (review R1) carries no automated marker"},
		{"REQUEST_CHANGES with the first marker", markers, cr("R1", "bot-a", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "[synthetic-bot: automated review] 2 findings"), ""},
		{"REQUEST_CHANGES with the second marker", markers, cr("R1", "bot-b", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "Synthetic tally: 1 of 2"), ""},
		{"a partial marker is no marker", markers, cr("R1", "bot-a", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "[synthetic-bot: automated"),
			model.ApproveBlocked + "a REQUEST_CHANGES by bot-a (review R1) carries no automated marker"},
		{"the marker in another case is no marker", markers, cr("R1", "bot-b", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "synthetic TALLY: 1 of 2"),
			model.ApproveBlocked + "a REQUEST_CHANGES by bot-b (review R1) carries no automated marker"},
		{"no markers configured: the automated one blocks too", nil, cr("R1", "bot-a", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "[synthetic-bot: automated review] 2 findings"),
			model.ApproveBlocked + "a REQUEST_CHANGES by bot-a (review R1) carries no automated marker"},
		{"an empty marker exempts nothing", []string{""}, cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "Please split this."),
			model.ApproveBlocked + "a REQUEST_CHANGES by reviewer-a (review R1) carries no automated marker"},
		{"superseded by the same author's approval", markers, both(cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "Please split this."),
			cr("R2", "reviewer-a", "2026-11-20T02:00:00Z", "APPROVED", "ok now")), ""},
		{"a later comment does not lift it", markers, both(cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "Please split this."),
			cr("R2", "reviewer-a", "2026-11-20T02:00:00Z", "COMMENTED", "still looking")),
			model.ApproveBlocked + "a REQUEST_CHANGES by reviewer-a (review R1) carries no automated marker"},
		{"dismissed", markers, cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "DISMISSED", "Please split this."), ""},
		{"an unresolved thread marked blocking", markers, blockingThread("Blocking: this drops the error.", false),
			model.ApproveBlocked + "unresolved thread PRRT_2 (b.go) is marked blocking by reviewer-b with no automated marker"},
		{"the same thread resolved", markers, blockingThread("Blocking: this drops the error.", true), ""},
		{"a thread marked non-blocking", markers, blockingThread("non-blocking: style only; not a blocker", false), ""},
		{"a blocking thread with a marker", markers, blockingThread("Synthetic tally: BLOCKING 1", false), ""},
		{"not read whole", markers, func(pr *model.PRState) { pr.Complete = false },
			model.ApproveBlocked + "the PR was not read whole this round (reviews or threads past the read could block)"},
		{"two blockers, in order", markers, both(cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "x"), blockingThread("blocker", false)),
			model.ApproveBlocked + "a REQUEST_CHANGES by reviewer-a (review R1) carries no automated marker; unresolved thread PRRT_2 (b.go) is marked blocking by reviewer-b with no automated marker"},
	} {
		w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true, ApprovePRs: []string{"acme/widget#1@abc123"},
			AutomatedMarkers: tc.markers, Maintainer: "@0000000a"}
		snap := head(tc.mut)
		post := &model.Post{PR: "acme/widget#1", Body: "approving", Event: "APPROVE"}
		if why := Authorize(w, snap, "jack", post); why != "" {
			t.Fatalf("%s: Authorize refused %q", tc.name, why)
		}
		why, alarm, wake := approveGate(w, snap, "p1", post, t0Gate)
		if why != tc.want {
			t.Errorf("%s: refusal\n got %q\nwant %q", tc.name, why, tc.want)
			continue
		}
		if tc.want == "" {
			if alarm != nil || wake != nil {
				t.Errorf("%s: alarm %s wake %+v on an approval that goes out", tc.name, alarm, wake)
			}
			continue
		}
		if want := `{"at":"2026-11-20T05:00:00Z","detail":` + string(model.MustJSON(tc.want)) + `,"head":"abc123","kind":"approve-blocked"}`; string(alarm) != want {
			t.Errorf("%s: alarm\n got %s\nwant %s", tc.name, alarm, want)
		}
		if wake == nil || wake.To != "@0000000a" || !strings.HasPrefix(wake.Text, "ebac: approval blocked, rev post p1, acme/widget#1@abc123: ") || strings.ContainsAny(wake.Text, "\n\r") || len(wake.Text) > 400 {
			t.Errorf("%s: wake %+v", tc.name, wake)
		}
	}
	// No trusted maintainer: refused and alarmed, nobody woken. A comment is never gated.
	w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true, ApprovePRs: []string{"acme/widget#1@abc123"}}
	snap := head(cr("R1", "Reviewer-A", "2026-11-20T01:00:00Z", "CHANGES_REQUESTED", "x"))
	if why, alarm, wake := approveGate(w, snap, "p1", &model.Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, t0Gate); why == "" || alarm == nil || wake != nil {
		t.Errorf("no maintainer: %q %s %+v", why, alarm, wake)
	}
	if why, _, _ := approveGate(w, snap, "p2", &model.Post{PR: "acme/widget#1", Body: "x"}, t0Gate); why != "" {
		t.Errorf("a comment was gated: %q", why)
	}
}

var t0Gate = time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)

// The core's half: an approval the plugin's fresh read blocked at send time gets the receipt alarm and
// one wake, like one the core blocked; a plugin refusal for another reason, or a comment, gets neither. RED if
// pluginBlocked goes or stops matching the prefix.
func TestAPluginBlockedApprovalIsAlarmedOnce(t *testing.T) {
	_, snap := outboxCritic()
	w := &model.Critic{Name: "rev", Maintainer: "@0000000a"}
	todo := map[string]*model.Post{
		"p1": {PR: "acme/widget#1", Event: "APPROVE", Refused: model.ApproveBlocked + "unresolved thread PRRT_1 (a.go) is marked blocking by b with no automated marker"},
		"p2": {PR: "acme/widget#1", Event: "APPROVE", Refused: "the head moved to x since the approval was authorized at abc123: not approved"},
		"p3": {PR: "acme/widget#1", Body: "x", Refused: model.ApproveBlocked + "x"},
	}
	alarms := map[string]json.RawMessage{}
	wakes := pluginBlocked(w, snap, []string{"p1", "p2", "p3"}, todo, alarms, t0Gate)
	if len(wakes) != 1 || wakes[0].To != "@0000000a" || wakes[0].Text != "ebac: approval blocked, rev post p1, acme/widget#1@abc123: unresolved thread PRRT_1 (a.go) is marked blocking by b with no automated marker (1 blocker(s); receipt post.p1.alarm)" {
		t.Fatalf("wakes %+v", wakes)
	}
	if len(alarms) != 1 || alarms["p1"] == nil {
		t.Fatalf("alarms %v", alarms)
	}
}

// The wiring: a post the plugin refuses with model.ApproveBlocked comes back through postJudged as its alarm and
// one wake. RED if postJudged stops judging the plugin's results (mutant S45).
func TestPostJudgedAlarmsWhatThePluginBlocked(t *testing.T) {
	_, snap := outboxCritic()
	handle := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		out := map[string]*model.Post{}
		for id, p := range req.Posts {
			c := *p
			c.Refused = model.ApproveBlocked + "a REQUEST_CHANGES by b (review R1) carries no automated marker"
			out[id] = &c
		}
		return &plugin.Reply{Posts: out}, nil
	}
	var log strings.Builder
	p := &Poller{Out: &log, Plugins: plugin.InProc{"gh": handle}}
	w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Maintainer: "@0000000a"}
	todo := map[string]*model.Post{"p1": {PR: "acme/widget#1", Body: "x", Event: "APPROVE"}}
	alarms := map[string]json.RawMessage{}
	wakes := p.postJudged(context.Background(), w, snap, []string{"p1"}, todo, alarms, t0Gate)
	if len(wakes) != 1 || alarms["p1"] == nil || !strings.HasPrefix(todo["p1"].Refused, model.ApproveBlocked) {
		t.Fatalf("wakes %+v alarms %v post %+v log %q", wakes, alarms, todo["p1"], log.String())
	}
}
