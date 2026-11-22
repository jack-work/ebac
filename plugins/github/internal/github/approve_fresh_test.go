package github

import (
	"context"
	"errors"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// An APPROVE is judged on a fresh full read at send time, never on the round's snapshot. The stale
// row is what a PR reused from the stored form looks like (comment bodies dropped, digests kept): the snapshot rule
// passes it (the bug), the fresh read blocks it. Controls: a clean fresh read approves; a failed or partial read
// retries (Error, never approved); a moved head is refused. RED if approveFresh is skipped or judges the snapshot.
func TestApprovalRechecksBlockingBodiesAfterSnapshotReload(t *testing.T) {
	pr := func(body string) *model.PRState {
		c := &model.Comment{ID: "C1", Author: "reviewer-b", Body: body}
		c.ComputeDigest()
		return &model.PRState{Key: "acme/widget#1", Owner: "acme", Repo: "widget", Number: 1, HeadSHA: "abc123", Complete: true,
			Reviews: map[string]*model.Review{},
			Threads: map[string]*model.Thread{"PRRT_1": {ID: "PRRT_1", Path: "a.go", Comments: []*model.Comment{c}}}}
	}
	stale := pr("Blocking: this drops the error.")
	stale.Threads["PRRT_1"].Comments[0].Body = "" // as reloaded from the form: digest only
	if bs := model.ApproveBlockers(stale, nil); len(bs) != 0 {
		t.Fatalf("fixture: the stale snapshot already blocks (%v), it cannot show the bug", bs)
	}
	w := &model.Critic{Name: "rev", AutomatedMarkers: []string{"[synthetic-bot]"}}
	for _, tc := range []struct {
		name          string
		fresh         *model.PRState
		err           error
		ok            bool
		refused, fail string
	}{
		{"fresh read shows the blocking thread", pr("Blocking: this drops the error."), nil, false,
			model.ApproveBlocked + "unresolved thread PRRT_1 (a.go) is marked blocking by reviewer-b with no automated marker", ""},
		{"fresh read clean", pr("nit: rename"), nil, true, "", ""},
		{"read failed", nil, errors.New("gh: Server Error (HTTP 502)"), false, "", "fresh read before the approval failed: gh: Server Error (HTTP 502)"},
		{"partial read", func() *model.PRState { p := pr("nit"); p.Complete = false; return p }(), nil, false, "", "fresh read before the approval was partial: not approved this round"},
		{"head moved", func() *model.PRState { p := pr("nit"); p.HeadSHA = "def456"; return p }(), nil, false,
			"the head moved to def456 since the approval was authorized at abc123: not approved", ""},
	} {
		calls := 0
		fetch := func(_ context.Context, owner, repo string, n int) (*model.PRState, error) {
			calls++
			if owner != "acme" || repo != "widget" || n != 1 {
				t.Fatalf("%s: fetched %s/%s#%d", tc.name, owner, repo, n)
			}
			return tc.fresh, tc.err
		}
		post := &model.Post{PR: "acme/widget#1", Body: "approving", Event: "APPROVE"}
		got := approveFresh(context.Background(), fetch, w, stale, post)
		if got != tc.ok || post.Refused != tc.refused || post.Error != tc.fail || calls != 1 {
			t.Errorf("%s: ok %v refused %q error %q calls %d", tc.name, got, post.Refused, post.Error, calls)
		}
	}
}
