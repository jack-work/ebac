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
	want := []string{"approver", "bin", "body-file", "clone", "config", "credential", "gh", "grant", "host", "keel-url", "notes", "principal", "socket", "token-cmd", "worktree-root"}
	if len(sudoRefused) != len(want) {
		t.Fatalf("%d refused flags, want %d (%v)", len(sudoRefused), len(want), want)
	}
	for _, w := range want {
		if _, ok := sudoRefused[w]; !ok {
			t.Errorf("%s is not refused", w)
		}
	}
}

// What passes through the door: harmless flags and --body-file - (stdin) in both forms. A refused name anywhere,
// also after "--" or as another flag's value, is refused (review d9d993be F1, F2: main's global extraction and Go's
// flag package both read past those). RED if the stdin exception goes or a "--" stops the scan.
func TestSudoDoorLetsHarmlessArgumentsThrough(t *testing.T) {
	for _, args := range [][]string{
		{"show", "--critic", "author-jack"},
		{"set", "--critic", "x", "--end", "expires=30d"},
		{"post", "--critic", "x", "--pr", "a/b#1", "--body-file", "-"},
		{"post", "--critic", "x", "--pr", "a/b#1", "--body-file=-"},
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

func TestSudoDoorScansPastDoubleDashAndValuePositions(t *testing.T) {
	for _, args := range [][]string{
		{"show", "--critic", "x", "--", "--socket", "/nonexistent/x.sock"},
		{"add-keel", "--name", "--", "--token-cmd", "X"},
		{"post", "--critic", "x", "--", "--gh", "/x"},
	} {
		if err := refuseSudoFlags("1000", args); err == nil {
			t.Errorf("%v let through", args)
		}
	}
}

// Under the door PATH keeps only store, system and wrapper entries (sudo keeps the caller's PATH: review F3).
// RED if a caller directory survives or a trusted one is dropped.
func TestSudoPathKeepsOnlyTrustedEntries(t *testing.T) {
	in := "/home/gluck/bin:/nix/store/abc-gh/bin:/tmp/evil:/run/current-system/sw/bin:/run/wrappers/bin:/etc/profiles/per-user/ebac/bin:/etc/profiles/per-user/gluck/bin:/run/current-system/sw/binX"
	want := "/nix/store/abc-gh/bin:/run/current-system/sw/bin:/run/wrappers/bin:/etc/profiles/per-user/ebac/bin"
	if got := sudoPath(in); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// main's gate: with SUDO_UID it refuses a path flag and narrows PATH; without it, it does neither. RED if main stops
// calling the scan, or the PATH narrowing goes (review F7: the wiring was untested).
func TestSudoGateWiring(t *testing.T) {
	env := map[string]string{"SUDO_UID": "1000", "PATH": "/tmp/evil:/nix/store/x/bin"}
	get := func(k string) string { return env[k] }
	set := func(k, v string) error { env[k] = v; return nil }
	if err := sudoGate([]string{"show", "--socket", "/x"}, get, set); err == nil {
		t.Fatal("a path flag passed the gate under sudo")
	}
	if err := sudoGate([]string{"show", "--critic", "x"}, get, set); err != nil || env["PATH"] != "/nix/store/x/bin" {
		t.Fatalf("under sudo: err %v PATH %q", err, env["PATH"])
	}
	env = map[string]string{"PATH": "/tmp/evil"}
	if err := sudoGate([]string{"show", "--socket", "/x"}, get, set); err != nil || env["PATH"] != "/tmp/evil" {
		t.Fatalf("without sudo: err %v PATH %q", err, env["PATH"])
	}
}
