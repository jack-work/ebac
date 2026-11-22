package disclose

import (
	"strings"
	"testing"
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
}
