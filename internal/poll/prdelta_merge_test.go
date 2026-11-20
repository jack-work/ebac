package poll

import (
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// Observed live in production use: one reviewer's single review
// action produced NINE distinct GraphQL review nodes on the same PR by the
// same author (one CHANGES_REQUESTED plus eight COMMENTED, one per inline
// thread). EvReviewSubmitted carried no field that distinguished them --
// same Kind, same PR, same empty ThreadID/Comment, same Detail ("COMMENTED")
// -- so mergeEvents' dedup key collapsed all eight COMMENTED submissions
// into one. Seven real review events vanished with no trace: not elided
// (the elide counter only fires past the 12-event projection cap), not
// suppressed, just gone before the delta was even built.
func TestDistinctReviewsWithSameStateAreNotDeduplicated(t *testing.T) {
	var fresh []model.Event
	for i := 0; i < 8; i++ {
		fresh = append(fresh, model.Event{
			Kind: model.EvReviewSubmitted, Tier: model.TierRecord, PR: "acme/widget#23654",
			Author: "bob", Detail: "COMMENTED", ReviewID: string(rune('a' + i)),
		})
	}
	got := MergeEvents(nil, fresh)
	if len(got) != 8 {
		t.Fatalf("8 distinct reviews collapsed to %d: %v", len(got), got)
	}
}
