package main

import (
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func TestReapPromotedClosesItemsOfFinishedCritics(t *testing.T) {
	critics := []*model.Critic{{Name: "live"}, {Name: "gone-soon", Runtime: model.Runtime{Archived: true}}, {Name: "paused", Runtime: model.Runtime{Stopped: true}}}
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
	// A Teams chat or author watch is not a PR review: it holds no queue slot.
	critics = append(critics, &model.Critic{Name: "chat", Mode: model.ModeTeams, Source: "teams-chat"},
		&model.Critic{Name: "author", Mode: model.ModeAuthor, Source: "github-author"}, &model.Critic{Name: "pr2", Mode: model.ModeReview})
	if n := activeCritics(critics); n != 2 {
		t.Fatalf("active = %d; want 2 (live + pr2): non-PR sources must not hold a slot", n)
	}
}
