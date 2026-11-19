package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/model"
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

// main's gate: with SUDO_UID it refuses a path flag and REPLACES PATH whole with sudoPATH (a caller "..", store or
// /tmp entry never survives: reviews F3, N1, N2); without it, it does neither. RED if main stops calling the scan,
// or PATH is filtered or kept instead of replaced.
func TestSudoGateWiring(t *testing.T) {
	env := map[string]string{"SUDO_UID": "1000", "PATH": "/run/current-system/sw/bin/../../../../tmp/X:/nix/store/fake-systemctl/bin:/tmp/evil"}
	get := func(k string) string { return env[k] }
	set := func(k, v string) error { env[k] = v; return nil }
	if err := sudoGate([]string{"show", "--socket", "/x"}, get, set); err == nil {
		t.Fatal("a path flag passed the gate under sudo")
	}
	if err := sudoGate([]string{"show", "--critic", "x"}, get, set); err != nil || env["PATH"] != sudoPATH || sudoPATH != "/run/current-system/sw/bin:/run/wrappers/bin" {
		t.Fatalf("under sudo: err %v PATH %q", err, env["PATH"])
	}
	env = map[string]string{"PATH": "/tmp/evil"}
	if err := sudoGate([]string{"show", "--socket", "/x"}, get, set); err != nil || env["PATH"] != "/tmp/evil" {
		t.Fatalf("without sudo: err %v PATH %q", err, env["PATH"])
	}
}

// TestMainHelper runs main() with EBAC_MAIN_ARGS when EBAC_MAIN_HELPER=1 (used by TestMainRefusesUnderSudo).
func TestMainHelper(t *testing.T) {
	if os.Getenv("EBAC_MAIN_HELPER") != "1" {
		t.Skip("helper: runs only as a subprocess of TestMainRefusesUnderSudo")
	}
	os.Args = append([]string{"ebac"}, strings.Fields(os.Getenv("EBAC_MAIN_ARGS"))...)
	main()
}

// The real binary's main refuses a path flag under SUDO_UID with exit 2 and the door's message, before it dials
// anything. RED if main stops calling the gate (review F7).
func TestMainRefusesUnderSudo(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelper$")
	cmd.Env = append(os.Environ(), "EBAC_MAIN_HELPER=1", "SUDO_UID=1000", "EBAC_MAIN_ARGS=show --critic x --socket /nonexistent/x.sock")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "--socket refused through the sudo door (SUDO_UID=1000)") {
		t.Fatalf("exit %v, output:\n%s", err, out)
	}
}

// main's gate on the real environment replaces PATH (not just refuses flags): RED if main's gate stops calling
// sudoGate (review 21cfc5d0 F7 residual: the subprocess test only saw the refusal).
func TestMainGateReplacesPath(t *testing.T) {
	t.Setenv("SUDO_UID", "1000")
	t.Setenv("PATH", "/tmp/evil")
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{"ebac", "show", "--critic", "x"}
	if err := mainGate(); err != nil || os.Getenv("PATH") != sudoPATH {
		t.Fatalf("err %v PATH %q, want %q", err, os.Getenv("PATH"), sudoPATH)
	}
}

// Every ebac command the harness and the brief teach a seat must pass the sudo door: each --body-file in that text is
// "- < <file>" (the body on stdin), and the command, cut at the redirect, is accepted by the door. RED if any taught
// command goes back to --body-file <file> (a seat following the form would be refused).
func TestTaughtCommandsPassTheDoor(t *testing.T) {
	w := &model.Critic{Name: "review-1", Mode: model.ModeReviewer, IssueRepo: "a/b", FormID: "@f"}
	h := brief.NewHarnessContract(w)
	text := h.Post + "\n" + h.Issue + "\n" + brief.ProtocolText(w, &model.Snapshot{}, "me")
	dev := *w
	dev.Mode = model.ModeDevelop
	text += "\n" + brief.ProtocolText(&dev, &model.Snapshot{}, "me")
	var cmds []string
	for _, line := range strings.Split(text, "\n") {
		for _, part := range strings.Split(line, "`") {
			for _, verb := range []string{"ebac post ", "ebac issue "} {
				if i := strings.Index(part, verb); i >= 0 && strings.Contains(part[i:], "--body-file") {
					cmds = append(cmds, part[i:])
				}
			}
		}
	}
	if len(cmds) < 5 {
		t.Fatalf("found %d taught --body-file commands, want at least 5 (post, issue, review, inline, develop issue): %q", len(cmds), cmds)
	}
	for _, c := range cmds {
		if !strings.Contains(c, "--body-file - <") {
			t.Errorf("taught command does not pass the body on stdin: %q", c)
		}
		argv := strings.Fields(strings.SplitN(c, "<", 2)[0])
		if err := refuseSudoFlags("1000", argv[1:]); err != nil {
			t.Errorf("taught command refused by the door: %q: %v", c, err)
		}
	}
}
