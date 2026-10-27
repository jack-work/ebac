package forge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func TestGHPostChoosesTheEndpoint(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("EBAC_FAKE_LOG", log)
	gh := fakeGH(t, "post-ok")
	_, snap := outboxCritic()
	pr := snap.PRs["acme/widget#1"]
	for _, p := range []*model.Post{
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
