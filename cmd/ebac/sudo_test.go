package main

import (
	"strings"
	"testing"
)

// One row per refused flag, in both spellings and both value forms: refused through the sudo door, accepted without
// it. RED if a flag leaves the list, a spelling slips past the scan, or the door is not keyed on SUDO_UID.
func TestSudoDoorRefusesEveryPathFlag(t *testing.T) {
	for name := range sudoRefused {
		for _, args := range [][]string{
			{"show", "--" + name, "/x"},
			{"show", "--" + name + "=/x"},
			{"show", "-" + name, "/x"},
			{"show", "-" + name + "=/x"},
			{"add-keel", "--repo", "a/b", "--" + name, "/x"},
		} {
			err := refuseSudoFlags("1000", args)
			if err == nil || !strings.Contains(err.Error(), "--"+name+" refused through the sudo door (SUDO_UID=1000)") {
				t.Errorf("%v under sudo: %v", args, err)
			}
			if err := refuseSudoFlags("", args); err != nil {
				t.Errorf("%v without sudo: %v", args, err)
			}
		}
	}
	// The census: every flag the audit found that picks a binary, socket, host, URL, config, credential or path.
	want := []string{"bin", "body-file", "clone", "config", "gh", "host", "keel-url", "notes", "socket", "token-cmd", "worktree-root"}
	if len(sudoRefused) != len(want) {
		t.Fatalf("%d refused flags, want %d (%v)", len(sudoRefused), len(want), want)
	}
	for _, w := range want {
		if _, ok := sudoRefused[w]; !ok {
			t.Errorf("%s is not refused", w)
		}
	}
}

// What passes through the door: harmless flags, --body-file - (stdin) in both forms, a refused flag's NAME as a
// value, and anything after "--". RED if the stdin exception, the value/flag distinction or "--" goes.
func TestSudoDoorLetsHarmlessArgumentsThrough(t *testing.T) {
	for _, args := range [][]string{
		{"show", "--critic", "author-jack"},
		{"set", "--critic", "x", "--end", "expires=30d"},
		{"post", "--critic", "x", "--pr", "a/b#1", "--body-file", "-"},
		{"post", "--critic", "x", "--pr", "a/b#1", "--body-file=-"},
		{"post", "--critic", "x", "--", "--gh", "/x"},
		{"ls"},
	} {
		if err := refuseSudoFlags("1000", args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	// A refused flag's name given as another flag's VALUE is still a token starting with "--": the scan refuses it
	// (fail closed), which is the safe side; this row pins that choice.
	if err := refuseSudoFlags("1000", []string{"stop", "--critic", "x", "--reason", "--socket"}); err == nil {
		t.Error("a '--socket' token was let through")
	}
	if err := refuseSudoFlags("1000", []string{"post", "--body-file", "/var/lib/ebac/x"}); err == nil {
		t.Error("--body-file with a path was let through")
	}
}
