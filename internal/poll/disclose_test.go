package poll

import (
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/model"
)

// Authorize refuses a disclosing body and passes the same post clean (the term list: sdk/disclose).
func TestAuthorizeRefusesDisclosure(t *testing.T) {
	// Authorize refuses a disclosing body and passes the same post clean.
	w := &model.Critic{Name: "rw", Write: true}
	snap := &model.Snapshot{PRs: map[model.PRKey]*model.PRState{"acme/widget#1": {Key: "acme/widget#1"}}}
	if got := Authorize(w, snap, "jack", &model.Post{PR: "acme/widget#1", Body: "checked on gluk"}); !strings.Contains(got, `"gluk"`) {
		t.Errorf("disclosing body: %q", got)
	}
	// The control for backlog #34 (keel sends the same body: TestAuthorizeKeelArms): to a GHE PR it is refused.
	if got := Authorize(w, snap, "jack", &model.Post{PR: "acme/widget#1", Body: "queued on band-hub, see the captains-log"}); !strings.Contains(got, "internal term") {
		t.Errorf("GHE body naming band-hub: %q, want a refusal", got)
	}
	if got := Authorize(w, snap, "jack", &model.Post{PR: "acme/widget#1", Body: "checked locally"}); got != "" {
		t.Errorf("clean body refused: %q", got)
	}
}

func TestPostMarkerNeutral(t *testing.T) {
	m := model.PostMarker("keel-decisions-minister", "p123")
	if disclose.Disclosure(m) != "" || strings.Contains(m, "minister") || strings.Contains(m, "p123") {
		t.Fatalf("marker leaks: %s", m)
	}
	if m == model.PostMarker("keel-decisions-minister", "p124") {
		t.Fatal("marker does not depend on the post id")
	}
}
