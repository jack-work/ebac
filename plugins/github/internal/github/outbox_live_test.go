package github

// The outbox's live tests: the core's poller (internal/poll) flushes posts
// through this plugin, in process, against a fake gh. They live with the
// plugin because the core no longer imports it (step 4).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/livetest"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/sdk/forge/forgetest"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

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

// Live tests run against the real angelus. They are gated because they mint
// and kill real arias; run with:
//
//	EBAC_LIVE=1 go test -run Live ./...
//
// They exist because the two things the SDK conversion changed most are the
// two that cannot be unit-tested: whether a role-targeted send still reaches
// the holder, and whether a conditional write actually refuses a stale base.
// Both were previously "should work" and both are load-bearing.
func liveOrSkip(t *testing.T) *figaro.Figaro {
	t.Helper()
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	return f
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
	p := &poll.Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.Plugins = plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, scenario)}.Handle}
		acks := p.FlushOutbox(ctx, w, snap)
		if err := st.SaveRecordingForTest(ctx, w, snap, acks); err != nil {
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
		Body: "again\n\n" + model.PostMarker(w.Name, "p2")}
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
	p := &poll.Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.Plugins = plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, scenario)}.Handle}
		if err := st.SaveRecordingForTest(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
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
	p := &poll.Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.Plugins = plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, scenario)}.Handle}
		if err := st.SaveRecordingForTest(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
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
	p := &poll.Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack"}
	round := func(scenario string) {
		p.Plugins = plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, scenario)}.Handle}
		if err := st.SaveRecordingForTest(ctx, w, snap, p.FlushOutbox(ctx, w, snap)); err != nil {
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
