package github

import (
	"context"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/forge/forgetest"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/prdelta"
)

// 2026-10-20: a pending review swallowed 344 thread replies on bic/aether#56134
// while ebac recorded every one as sent.
func TestFetchPRSeesPendingReviewsAndPendingComments(t *testing.T) {
	g := forgetest.FakeGH(t, "pending")
	pr, err := g.FetchPR(context.Background(), "acme", "widget", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Pending) != 1 || pr.Pending[0].DatabaseID != 213120916 || pr.Pending[0].Comments != 344 || pr.Pending[0].Author != "jack" {
		t.Fatalf("pending = %+v", pr.Pending)
	}
	cs := pr.Threads["T1"].Comments
	if cs[0].State != "" || cs[1].State != "PENDING" {
		t.Fatalf("comment states %q %q; want published, PENDING", cs[0].State, cs[1].State)
	}
	if !pr.Threads["T1"].AwaitingUs("jack") {
		t.Error("a reply held in a pending review counted as an answer")
	}
	if _, ok := model.FindMarker(pr, "jack", model.PostMarker("rw", "p1")); ok {
		t.Error("a pending comment was recovered as sent")
	}
	d := prdelta.Diff(&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: {Key: pr.Key, State: "OPEN", HeadSHA: "sha1", Threads: pr.Threads}}},
		&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}, Complete: true}, "jack")
	var got []model.Event
	for _, e := range d.Events {
		if e.Kind == model.EvPendingReview {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Tier != model.TierWake || !strings.Contains(got[0].Detail, "213120916") {
		t.Fatalf("pending review events %+v; want one wake naming 213120916", got)
	}
	gone := prdelta.Diff(&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}, Complete: true},
		&model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: {Key: pr.Key, State: "OPEN", HeadSHA: "sha1", Threads: pr.Threads}}, Complete: true}, "jack")
	found := false
	for _, e := range gone.Events {
		found = found || e.Kind == model.EvPendingReviewGone
	}
	if !found {
		t.Error("the pending review going away was not recorded")
	}
}
