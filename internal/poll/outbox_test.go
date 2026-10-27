package poll

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
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
	rev := model.Critic{Name: "rev", Mode: model.ModeReviewer, Approve: true}
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
		{"reviewer resolves", &rev, "jack", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true}, "never resolves"},
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
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w, snap := outboxCritic()
	w.Name = "livetest-" + strings.TrimPrefix(id, "@")[:8]
	w.Write = write
	w.FormID = id
	w.Runtime.Round = 7
	snap.Critic, snap.Round, snap.TakenAt, snap.Complete = w.Name, 7, time.Now().UTC().Truncate(time.Second), true
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments = []*model.Comment{{ID: "C1", Author: "friend", Body: "first", CreatedAt: "2026-10-20T01:00:00Z"}}
	if err := st.Save(ctx, w, snap, nil); err != nil {
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
	if err := st.Save(ctx, w, snap, nil); err != nil {
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
	for _, k := range seen {
		if !strings.HasPrefix(k, "prs.acme/widget#1.threads.PRRT_1.c.C2.") {
			t.Errorf("seat-visible change outside the new comment: %s", k)
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

func TestLiveOutboxPostsOnceAcksAndRetries(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	calls := func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "CALL ")
	}
	p := &Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.GH = forgetest.FakeGH(t, scenario)
		acks := p.FlushOutbox(ctx, w, snap)
		if err := st.Save(ctx, w, snap, acks); err != nil {
			t.Fatal(err)
		}
	}

	writePost(t, st, w, "p1", model.Post{PR: "acme/widget#1", Body: "hello", ReplyTo: "PRRT_1"})
	round("post-fail")
	if got := readPost(t, f, w, "p1"); got.Sent != nil || got.Error == "" || got.Attempts != 1 {
		t.Fatalf("after a failed send: %+v", got)
	}
	failedAt := w.Runtime.OutboxWatermark

	round("post-ok")
	got := readPost(t, f, w, "p1")
	if got.Sent == nil || got.Sent.URL != "https://x/pull/1#posted" || got.Error != "" {
		t.Fatalf("retry did not land: %+v", got)
	}
	if calls() != 1 {
		t.Fatalf("%d forge calls after the retry, want 1", calls())
	}
	if w.Runtime.OutboxWatermark <= failedAt {
		t.Fatalf("watermark %d did not pass %d once the post landed", w.Runtime.OutboxWatermark, failedAt)
	}

	round("post-ok")
	if calls() != 1 {
		t.Fatalf("a sent post was sent again: %d calls", calls())
	}

	// A post that reached the forge but was never acked is found by its marker.
	writePost(t, st, w, "p2", model.Post{PR: "acme/widget#1", Body: "again"})
	snap.PRs["acme/widget#1"].Comments["IC9"] = &model.Comment{ID: "IC9", Author: "jack", URL: "https://x/pull/1#ic9",
		Body: "again\n\n" + postMarker(w.Name, "p2")}
	round("post-ok")
	if got := readPost(t, f, w, "p2"); got.Sent == nil || !got.Sent.Recovered || got.Sent.URL != "https://x/pull/1#ic9" {
		t.Fatalf("marker not recovered: %+v", got)
	}
	if calls() != 1 {
		t.Fatalf("a recovered post was posted: %d calls", calls())
	}

	// Authority is checked at send time too.
	w.Write = false
	writePost(t, st, w, "p3", model.Post{PR: "acme/widget#1", Body: "nope"})
	round("post-ok")
	if got := readPost(t, f, w, "p3"); got.Refused == "" || got.Sent != nil {
		t.Fatalf("a read-only critic's post was not refused: %+v", got)
	}
	if calls() != 1 {
		t.Fatalf("a refused post reached the forge: %d calls", calls())
	}
}

func TestLiveOutboxResolvesThreads(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	calls := func(kind string) int {
		b, _ := os.ReadFile(log)
		n := 0
		for _, c := range strings.Split(string(b), "CALL ")[1:] {
			if strings.Contains(c, kind) {
				n++
			}
		}
		return n
	}
	p := &Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.GH = forgetest.FakeGH(t, scenario)
		if err := st.Save(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
			t.Fatal(err)
		}
	}

	writePost(t, st, w, "r1", model.Post{PR: "acme/widget#1", Body: "Fixed at abc.", ReplyTo: "PRRT_1", Resolve: true})
	round("post-ok")
	if got := readPost(t, f, w, "r1"); got.Sent == nil || got.Resolved == "" || got.Error != "" {
		t.Fatalf("reply+resolve: %+v", got)
	}
	if calls("addPullRequestReviewThreadReply") != 1 || calls("resolveReviewThread") != 1 {
		t.Fatalf("reply+resolve made %d replies, %d resolves", calls("addPullRequestReviewThreadReply"), calls("resolveReviewThread"))
	}

	writePost(t, st, w, "r2", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true})
	round("post-ok")
	if got := readPost(t, f, w, "r2"); got.Sent != nil || got.Resolved == "" {
		t.Fatalf("resolve alone: %+v", got)
	}
	if calls("addPullRequestReviewThreadReply") != 1 || calls("resolveReviewThread") != 2 {
		t.Fatalf("resolve alone posted a reply or skipped the resolve")
	}

	// The reply lands, the resolve fails: next round resolves without replying again.
	writePost(t, st, w, "r3", model.Post{PR: "acme/widget#1", Body: "Declined: out of scope.", ReplyTo: "PRRT_1", Resolve: true})
	round("resolve-fail")
	if got := readPost(t, f, w, "r3"); got.Sent == nil || got.Resolved != "" || got.Error == "" {
		t.Fatalf("after a failed resolve: %+v", got)
	}
	round("post-ok")
	if got := readPost(t, f, w, "r3"); got.Resolved == "" || got.Error != "" {
		t.Fatalf("resolve not retried: %+v", got)
	}
	if calls("addPullRequestReviewThreadReply") != 2 || calls("resolveReviewThread") != 3 {
		t.Fatalf("retry: %d replies (want 2), %d resolves (want 3)", calls("addPullRequestReviewThreadReply"), calls("resolveReviewThread"))
	}

	// Already resolved on the forge: acked without a call.
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].IsResolved = true
	writePost(t, st, w, "r4", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", Resolve: true})
	round("post-ok")
	if got := readPost(t, f, w, "r4"); got.Resolved == "" {
		t.Fatalf("already-resolved thread not acked: %+v", got)
	}
	if calls("resolveReviewThread") != 3 {
		t.Fatalf("an already-resolved thread was resolved again")
	}
}

func TestLiveOutboxReactsToTheFinding(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	callsOf := func(kind string) []string {
		b, _ := os.ReadFile(log)
		var out []string
		for _, c := range strings.Split(string(b), "CALL ")[1:] {
			if strings.Contains(c, kind) {
				out = append(out, c)
			}
		}
		return out
	}
	p := &Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.GH = forgetest.FakeGH(t, scenario)
		if err := st.Save(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
			t.Fatal(err)
		}
	}

	th := snap.PRs["acme/widget#1"].Threads["PRRT_1"]
	th.Comments = append(th.Comments, &model.Comment{ID: "C2", Author: "jack", Body: "reply", CreatedAt: "2026-10-20T03:00:00Z"})
	writePost(t, st, w, "k1", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "+1", Resolve: true})
	round("post-ok")
	got := readPost(t, f, w, "k1")
	if got.Reacted == "" || got.Resolved == "" || got.Sent != nil || got.Error != "" {
		t.Fatalf("react+resolve: %+v", got)
	}
	reacts := callsOf("addReaction")
	if len(reacts) != 1 || !strings.Contains(reacts[0], "s=C1") || !strings.Contains(reacts[0], "c=THUMBS_UP") {
		t.Fatalf("reaction calls %q; want one THUMBS_UP on C1, the comment that opened the thread", reacts)
	}
	if len(callsOf("addPullRequestReviewThreadReply")) != 0 {
		t.Fatal("a bodiless post replied")
	}

	// A failed reaction is retried; the resolve that follows waits for it.
	writePost(t, st, w, "k2", model.Post{PR: "acme/widget#1", ReplyTo: "PRRT_1", React: "heart", Resolve: true})
	round("post-fail")
	if got := readPost(t, f, w, "k2"); got.Reacted != "" || got.Resolved != "" || got.Error == "" {
		t.Fatalf("after a failed reaction: %+v", got)
	}
	round("post-ok")
	if got := readPost(t, f, w, "k2"); got.Reacted == "" || got.Resolved == "" {
		t.Fatalf("reaction not retried: %+v", got)
	}
}

func TestLiveOutboxNeverCallsAPendingReplySent(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	calls := func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "CALL ")
	}
	p := &Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.GH = forgetest.FakeGH(t, scenario)
		if err := st.Save(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
			t.Fatal(err)
		}
	}

	writePost(t, st, w, "q1", model.Post{PR: "acme/widget#1", Body: "Fixed.", ReplyTo: "PRRT_1"})
	round("reply-pending")
	got := readPost(t, f, w, "q1")
	if got.Sent != nil || !strings.Contains(got.Refused, "landed in pending review 213120916") {
		t.Fatalf("a pending reply: %+v", got)
	}
	round("post-ok")
	if calls() != 1 {
		t.Fatalf("a reply that landed pending was retried: %d calls", calls())
	}

	writePost(t, st, w, "q2", model.Post{PR: "acme/widget#1", Body: "Review.", Event: "COMMENT"})
	round("post-422")
	if got := readPost(t, f, w, "q2"); got.Attempts != 1 || !strings.Contains(got.Refused, "HTTP 422") {
		t.Fatalf("a 422 was not refused at once: %+v", got)
	}
}
