package poll

import (
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

func TestDisclosure(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"Fixed at 1a2b3c4; the local CI run is green and the failing test now passes.", ""},
		{"Reviewed by Figaro agent on behalf of Jack", ""},
		{"FIGARO checked this", ""},
		{"verified on the gluk stack", "gluk"},
		{"see keel aether/plan", "keel"},
		{"the boat keeled over; a stewardship model", ""},
		{"the Minister ruled; aria-label kept; the merge queue landed it; docs/notes/x.md; frequency band", ""},
		{"routed via ROU2", "ROU2"},
		{"posted by ebac", "ebac"},
		{"band-work gate-kit 5336", "band-work"},
		{"d6761919 forked it", ""},
		{"branch d6761919, a fork of the parent", ""},
		{"d6761919 is the seat that ran it", "d6761919"},
		{"see bandhub and the band hub", "bandhub"},
		{"from ~/notes/captains-log/x.md", "captains-log"},
	} {
		got := Disclosure(tc.body)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%q: got %q, want a refusal naming %q", tc.body, got, tc.want)
		}
	}
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
	if Disclosure(m) != "" || strings.Contains(m, "minister") || strings.Contains(m, "p123") {
		t.Fatalf("marker leaks: %s", m)
	}
	if m == model.PostMarker("keel-decisions-minister", "p124") {
		t.Fatal("marker does not depend on the post id")
	}
}
