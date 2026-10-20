package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func outboxCritic() (*Critic, *Snapshot) {
	pr := &PRState{
		Key: MakePRKey("acme", "widget", 1), Owner: "acme", Repo: "widget", Number: 1, URL: "https://x/pull/1",
		State: "OPEN", Author: "friend", HeadSHA: "abc123",
		Threads:  map[string]*Thread{"PRRT_1": {ID: "PRRT_1", Path: "a.go", Line: 3}},
		Reviews:  map[string]*Review{},
		Comments: map[string]*Comment{},
	}
	w := &Critic{Name: "rw", Mode: ModeReview, Write: true, FormID: "@f"}
	return w, &Snapshot{PRs: map[PRKey]*PRState{pr.Key: pr}}
}

func TestAuthorizeKeepsPostsInsideTheGrant(t *testing.T) {
	w, snap := outboxCritic()
	ro := *w
	ro.Write = false
	rev := Critic{Name: "rev", Mode: ModeReviewer, Approve: true}
	revNoApprove := Critic{Name: "rev", Mode: ModeReviewer}
	for _, tc := range []struct {
		name string
		w    *Critic
		me   string
		post Post
		want string // substring of the refusal; "" means allowed
	}{
		{"pr comment", w, "jack", Post{PR: "acme/widget#1", Body: "x"}, ""},
		{"thread reply", w, "jack", Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_1"}, ""},
		{"inline", w, "jack", Post{PR: "acme/widget#1", Body: "x", Path: "a.go", Line: 3}, ""},
		{"read-only critic", &ro, "jack", Post{PR: "acme/widget#1", Body: "x"}, "read-only"},
		{"foreign pr", w, "jack", Post{PR: "acme/other#9", Body: "x"}, "not a pull request of this critic"},
		{"empty", w, "jack", Post{PR: "acme/widget#1", Body: "  "}, "empty body"},
		{"unknown thread", w, "jack", Post{PR: "acme/widget#1", Body: "x", ReplyTo: "PRRT_9"}, "not on"},
		{"path without line", w, "jack", Post{PR: "acme/widget#1", Body: "x", Path: "a.go"}, "both path and line"},
		{"request changes", w, "jack", Post{PR: "acme/widget#1", Body: "x", Event: "REQUEST_CHANGES"}, "never allowed"},
		{"approve without grant", w, "jack", Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "not granted"},
		{"reviewer approves", &rev, "jack", Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, ""},
		{"reviewer without approve", &revNoApprove, "jack", Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "not granted"},
		{"approve own operator's pr", &rev, "friend", Post{PR: "acme/widget#1", Body: "x", Event: "APPROVE"}, "own operator"},
	} {
		got := authorize(tc.w, snap, tc.me, &tc.post)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusal %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGHPostChoosesTheEndpoint(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	gh := fakeGH(t, "post-ok")
	_, snap := outboxCritic()
	pr := snap.PRs["acme/widget#1"]
	for _, p := range []*Post{
		{Body: "c"},
		{Body: "r", ReplyTo: "PRRT_1"},
		{Body: "i", Path: "a.go", Line: 3},
		{Body: "a", Event: "APPROVE"},
	} {
		if url, err := gh.Post(context.Background(), pr, p, p.Body+" <!-- m -->"); err != nil || url != "https://x/pull/1#posted" {
			t.Fatalf("post %+v: url=%q err=%v", p, url, err)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(string(raw), "CALL ")[1:]
	if len(calls) != 4 {
		t.Fatalf("%d calls, want 4:\n%s", len(calls), raw)
	}
	for i, want := range []string{
		"api repos/acme/widget/issues/1/comments -X POST --input -",
		"addPullRequestReviewThreadReply",
		`api repos/acme/widget/pulls/1/reviews -X POST --input - -q .html_url	{"comments":[{"body":"i <!-- m -->","line":3,"path":"a.go","side":"RIGHT"}],"commit_id":"abc123","event":"COMMENT"}`,
		`{"body":"a <!-- m -->","commit_id":"abc123","event":"APPROVE"}`,
	} {
		if !strings.Contains(calls[i], want) {
			t.Errorf("call %d = %q, want it to contain %q", i, calls[i], want)
		}
	}
}

func TestPrsTreeRoundTripsAndOrdersComments(t *testing.T) {
	_, snap := outboxCritic()
	pr := snap.PRs["acme/widget#1"]
	pr.Threads["PRRT_1"].Comments = []*Comment{
		{ID: "C2", Author: "b", Body: "second", CreatedAt: "2026-10-20T02:00:00Z"},
		{ID: "C1", Author: "a", Body: "first", CreatedAt: "2026-10-20T01:00:00Z"},
	}
	pr.Comments["IC1"] = &Comment{ID: "IC1", Body: "top"}
	snap.PRs["acme/web.site#2"] = &PRState{Key: "acme/web.site#2", Owner: "acme", Repo: "web.site", Number: 2}

	raw := mustJSON(prsTree(snap))
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	for k := range keys {
		if strings.Contains(k, ".") {
			t.Fatalf("a dot in a repo name reached the form path: %q", k)
		}
	}
	got, err := prsFromTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["acme/web.site#2"]; !ok {
		t.Fatalf("dotted repo lost: keys %v", got)
	}
	cs := got["acme/widget#1"].Threads["PRRT_1"].Comments
	if len(cs) != 2 || cs[0].ID != "C1" || cs[1].ID != "C2" {
		t.Fatalf("thread comments not in created order: %+v", cs)
	}
	if got["acme/widget#1"].Comments["IC1"].Body != "top" {
		t.Error("PR comment lost")
	}
}

func TestReapPromotedClosesItemsOfFinishedCritics(t *testing.T) {
	critics := []*Critic{{Name: "live"}, {Name: "gone-soon", Runtime: Runtime{Archived: true}}, {Name: "paused", Runtime: Runtime{Stopped: true}}}
	items := []QueueItem{
		{ID: "a", State: QPromoted, Critic: "live"},
		{ID: "b", State: QPromoted, Critic: "gone-soon"},
		{ID: "c", State: QPromoted, Critic: "never-existed"},
		{ID: "d", State: QPending},
		{ID: "e", State: QPromoted, Critic: "paused"},
	}
	reaped := reapPromoted(items, critics)
	if len(reaped) != 2 || reaped[0].ID != "b" || reaped[1].ID != "c" || reaped[0].State != QDone {
		t.Fatalf("reaped %+v; want b and c marked done", reaped)
	}
	if n := activeCritics(critics); n != 1 {
		t.Fatalf("active = %d; a stopped or archived critic must not hold a slot", n)
	}
}

// ---------- live: the form is the state ----------

func liveCritic(t *testing.T, f *Figaro, write bool) (*Store, *Critic, *Snapshot) {
	t.Helper()
	ctx := context.Background()
	st, err := NewStore(f)
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
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments = []*Comment{{ID: "C1", Author: "friend", Body: "first", CreatedAt: "2026-10-20T01:00:00Z"}}
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
	before, err := st.readForm(ctx, w.FormID)
	if err != nil {
		t.Fatal(err)
	}
	snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments = append(snap.PRs["acme/widget#1"].Threads["PRRT_1"].Comments,
		&Comment{ID: "C2", Author: "amy", Body: "second", CreatedAt: "2026-10-20T02:00:00Z"})
	w.Runtime.Round = 8
	if err := st.Save(ctx, w, snap, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := f.node(ctx, w.FormID)
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
		if !strings.HasPrefix(k, "prs.acme/widget#1.threads.PRRT_1.comments.C2.") {
			t.Errorf("seat-visible change outside the new comment: %s", k)
		}
	}
	if len(seen) == 0 {
		t.Fatal("positive control: the new comment is not in the patch")
	}

	st2, _ := NewStore(NewFigaro(""))
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
	if cs := gs.PRs["acme/widget#1"].Threads["PRRT_1"].Comments; len(cs) != 2 || cs[1].Body != "second" {
		t.Fatalf("thread round-trip: %+v", cs)
	}
	form, _ := f.Form(ctx, w.FormID)
	if form["ack_note"] != "seat wrote this" {
		t.Errorf("seat key overwritten: %#v", form["ack_note"])
	}
}

func writePost(t *testing.T, st *Store, w *Critic, id string, p Post) {
	t.Helper()
	err := st.commit(context.Background(), w.FormID, func(cur map[string]json.RawMessage, _ ebacState) (map[string]json.RawMessage, []string) {
		posts := objectOf(cur["post"])
		posts[id] = mustJSON(p)
		return map[string]json.RawMessage{"post": mustJSON(posts)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readPost(t *testing.T, f *Figaro, w *Critic, id string) Post {
	t.Helper()
	form, err := f.Form(context.Background(), w.FormID)
	if err != nil {
		t.Fatal(err)
	}
	var posts map[string]Post
	_ = json.Unmarshal(mustJSON(form["post"]), &posts)
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
		p.GH = fakeGH(t, scenario)
		acks := p.flushOutbox(ctx, w, snap)
		if err := st.Save(ctx, w, snap, acks); err != nil {
			t.Fatal(err)
		}
	}

	writePost(t, st, w, "p1", Post{PR: "acme/widget#1", Body: "hello", ReplyTo: "PRRT_1"})
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
	writePost(t, st, w, "p2", Post{PR: "acme/widget#1", Body: "again"})
	snap.PRs["acme/widget#1"].Comments["IC9"] = &Comment{ID: "IC9", Author: "jack", URL: "https://x/pull/1#ic9",
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
	writePost(t, st, w, "p3", Post{PR: "acme/widget#1", Body: "nope"})
	round("post-ok")
	if got := readPost(t, f, w, "p3"); got.Refused == "" || got.Sent != nil {
		t.Fatalf("a read-only critic's post was not refused: %+v", got)
	}
	if calls() != 1 {
		t.Fatalf("a refused post reached the forge: %d calls", calls())
	}
}

// 2026-10-20: a deleted review and seven deleted comments reached the form as
// null leaves, and reading them back panicked every unit that loaded the PR.
func TestPrsFromTreeTreatsNullAsAbsent(t *testing.T) {
	raw := json.RawMessage(`{
	  "acme/widget#1": {"key":"acme/widget#1","owner":"acme","repo":"widget","number":1,
	    "threads": {
	      "T1": {"id":"T1","comments":{"C1":{"id":"C1","body":"kept","created_at":"1"},"C2":null,"C3":null}},
	      "T2": null
	    },
	    "reviews": {"R1":{"id":"R1","state":"APPROVED"},"R2":null},
	    "comments": {"I1":null}
	  },
	  "acme/gone#2": null
	}`)
	prs, err := prsFromTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %v", prs)
	}
	p := prs["acme/widget#1"]
	if len(p.Threads) != 1 || len(p.Threads["T1"].Comments) != 1 || p.Threads["T1"].Comments[0].ID != "C1" {
		t.Errorf("threads = %+v", p.Threads)
	}
	if len(p.Reviews) != 1 || p.Reviews["R1"] == nil || len(p.Comments) != 0 {
		t.Errorf("reviews = %v, comments = %v", p.Reviews, p.Comments)
	}
}
