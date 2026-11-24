package disclose

import (
	"fmt"
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
		{"the Lead ruled; aria-label kept; the merge queue landed it; docs/notes/x.md; frequency band", ""},
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

// The deployment's list (EBAC_DISCLOSE_NAMES) replaces the embedded one;
// set but unreadable or empty refuses to start. RED if the env is ignored or
// a bad file falls back to the embedded list.
func TestEnvListReplacesTheEmbeddedList(t *testing.T) {
	files := map[string]string{"/list": "# a deployment list\n\\bzorblax\\b\n", "/empty": "# nothing\n"}
	read := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("no such file %s", p)
	}
	env := func(v string) func(string) string { return func(string) string { return v } }
	ts, err := loadTerms(env("/list"), read)
	if err != nil || len(ts) != 1 || !ts[0].re.MatchString("a ZORBLAX ran") {
		t.Fatalf("env list: %v %v", ts, err)
	}
	if _, err := loadTerms(env("/missing"), read); err == nil {
		t.Fatal("an unreadable env list must refuse to start")
	}
	if _, err := loadTerms(env("/empty"), read); err == nil {
		t.Fatal("an empty env list must refuse to start")
	}
	if ts, err := loadTerms(env(""), read); err != nil || len(ts) != len(parseDisclose(discloseNames)) {
		t.Fatalf("unset: the embedded list, got %d terms %v", len(ts), err)
	}
}
