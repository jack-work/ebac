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
