package poll

import (
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func TestBackoffClimbsAndCaps(t *testing.T) {
	if backoffFor(1) != baseBackoff {
		t.Fatalf("first retry = %s", backoffFor(1))
	}
	if backoffFor(2) != 2*baseBackoff {
		t.Fatalf("second retry = %s", backoffFor(2))
	}
	if backoffFor(20) != maxBackoff {
		t.Fatalf("backoff did not cap: %s", backoffFor(20))
	}
}

func TestMergeEventsDeduplicatesAndCaps(t *testing.T) {
	e := model.Event{Kind: model.EvCommentNew, PR: "acme/widget#1", ThreadID: "t", Comment: "c"}
	got := MergeEvents([]model.Event{e}, []model.Event{e, e})
	if len(got) != 1 {
		t.Fatalf("dedup failed: %d events", len(got))
	}
	var many []model.Event
	for i := 0; i < 500; i++ {
		many = append(many, model.Event{Kind: model.EvCommentNew, Comment: string(rune(i))})
	}
	if n := len(MergeEvents(nil, many)); n > 200 {
		t.Fatalf("pending batch grew unbounded: %d", n)
	}
}
