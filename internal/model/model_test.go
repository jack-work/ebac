package model

import (
	"strings"
	"testing"
)

func TestParsePRRef(t *testing.T) {
	for _, tc := range []struct {
		in    string
		owner string
		repo  string
		num   int
		bad   bool
	}{
		{in: "https://github.com/acme/widget/pull/6622", owner: "acme", repo: "widget", num: 6622},
		{in: "acme/widget#42", owner: "acme", repo: "widget", num: 42},
		{in: "https://github.com/acme/widget/issues/12", bad: true},
		{in: "widget#1", bad: true},
		{in: "", bad: true},
	} {
		got, err := ParsePRRef(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q should not parse, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Owner != tc.owner || got.Repo != tc.repo || got.Number != tc.num {
			t.Errorf("%q -> %+v", tc.in, got)
		}
	}
}

func TestFirstLineTrimsOnRuneBoundary(t *testing.T) {
	in := strings.Repeat("é", 400)
	got := FirstLine(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("no ellipsis: %q", got[len(got)-8:])
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatal("trimmed mid-rune: replacement character in output")
	}
}

func TestValidNameRejectsSystemdHostileNames(t *testing.T) {
	for _, bad := range []string{"", "a b", "a/b", "a.b", strings.Repeat("x", 65)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
	for _, good := range []string{"widget", "aether-prs", "pr_6622"} {
		if err := ValidName(good); err != nil {
			t.Errorf("ValidName(%q): %v", good, err)
		}
	}
}

func TestValidAriaID(t *testing.T) {
	// `figaro bind -j` returns figaro_id, `figaro new -j` returns aria_id.
	// A script that reads the wrong field yields the literal "null", and
	// casting that produced an opaque xwal error. Refuse it up front.
	for _, bad := range []string{"", "null", "@1cb8a917", "not an id", "<nil>"} {
		if err := ValidAriaID(bad); err == nil {
			t.Errorf("ValidAriaID(%q) accepted", bad)
		}
	}
	for _, good := range []string{"0a0db7ec", "f37a50e5"} {
		if err := ValidAriaID(good); err != nil {
			t.Errorf("ValidAriaID(%q): %v", good, err)
		}
	}
}
