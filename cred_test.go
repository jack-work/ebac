package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const fakeTok = "gho_FAKE0123456789abcdef"

// fakeCredExec routes every child (band-cred, gh, tomb) to TestCredHelperProcess.
// The first child arg after "--" is the scenario; the rest is the real argv.
func fakeCredExec(t *testing.T, scenario string) {
	t.Helper()
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestCredHelperProcess", "--", scenario, name}, args...)...)
	}
	t.Cleanup(func() { execCommandContext = orig })
}

func TestCredHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 3 {
		return
	}
	scenario, bin, argv := args[1], args[2], args[3:]
	// band-cred token <name>
	if len(argv) == 2 && argv[0] == "token" {
		switch {
		case scenario == "stale":
			os.Stderr.WriteString("error: credential_stale:" + argv[1] + "\n")
			os.Exit(1)
		case argv[1] == credGitHub || argv[1] == credTeams:
			os.Stdout.WriteString(fakeTok + "\n")
			os.Exit(0)
		default:
			os.Stderr.WriteString("error: unknown credential " + argv[1] + "\n")
			os.Exit(1)
		}
	}
	switch scenario {
	case "gh-env": // the fake gh reports what it was given
		b, _ := json.Marshal(map[string]string{"bin": bin, "argv": strings.Join(argv, " "),
			"GH_TOKEN": os.Getenv("GH_TOKEN"), "GH_ENTERPRISE_TOKEN": os.Getenv("GH_ENTERPRISE_TOKEN"), "GH_HOST": os.Getenv("GH_HOST")})
		os.Stdout.Write(b)
		os.Exit(0)
	case "tomb-fd": // the fake tomb reads its token from TOMB_AAD_TOKEN_FD
		if os.Getenv("TOMB_AAD_TOKEN_FD") != "3" {
			os.Stderr.WriteString("Error: TOMB_AAD_TOKEN_FD=" + os.Getenv("TOMB_AAD_TOKEN_FD") + "\n")
			os.Exit(1)
		}
		got, _ := io.ReadAll(os.NewFile(3, "aad-token"))
		if string(got) != fakeTok {
			os.Stderr.WriteString("Error: fd 3 held " + string(got) + "\n")
			os.Exit(1)
		}
		if strings.Contains(strings.Join(argv, " "), fakeTok) || strings.Contains(strings.Join(os.Environ(), " "), fakeTok) {
			os.Stderr.WriteString("Error: token leaked into argv or env\n")
			os.Exit(1)
		}
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"fd-ok"}]}`)
		os.Exit(0)
	}
	os.Exit(2)
}

func TestCredTokenAndStale(t *testing.T) {
	ctx := context.Background()
	fakeCredExec(t, "ok")
	c := NewCred("band-cred", credGitHub)
	if tok, err := c.Token(ctx); err != nil || tok != fakeTok {
		t.Fatalf("token = %q, %v", tok, err)
	}
	fakeCredExec(t, "stale")
	if tok, err := c.Token(ctx); err != nil || tok != fakeTok {
		t.Fatalf("a fresh cached token must be reused within Reuse: %q %v", tok, err)
	}
	c2 := NewCred("band-cred", credGitHub)
	_, err := c2.Token(ctx)
	if err == nil || !strings.Contains(err.Error(), "band-cred login "+credGitHub) {
		t.Fatalf("stale must tell the Boss to sign in again: %v", err)
	}
}

func TestGHRunsWithTheHostsBandCredToken(t *testing.T) {
	ctx := context.Background()
	fakeCredExec(t, "gh-env")
	g := NewGH("gh", "microsoft.ghe.com")
	out, err := g.run(ctx, []string{"api", "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if got["GH_ENTERPRISE_TOKEN"] != fakeTok || got["GH_TOKEN"] != fakeTok || got["GH_HOST"] != "microsoft.ghe.com" {
		t.Fatalf("gh env: %v", got)
	}
	if strings.Contains(got["argv"], fakeTok) {
		t.Fatalf("token in argv: %q", got["argv"])
	}
	// Retargeting the host (the poller does) must not reuse another host's token.
	g.Host = "github.com"
	if _, err := g.run(ctx, []string{"api", "user"}, nil); err == nil || !strings.Contains(err.Error(), "no-credential-for-github.com") {
		t.Fatalf("a host without a credential must be refused, got %v", err)
	}
	// Control: the test seam really bypasses band-cred.
	g2 := NewGH("gh", "microsoft.ghe.com")
	g2.NoCred = true
	out, _ = g2.run(ctx, []string{"api", "user"}, nil)
	if strings.Contains(string(out), fakeTok) {
		t.Fatal("NoCred still passed a token")
	}
}

func TestTombGetsItsTokenOnFd3(t *testing.T) {
	ctx := context.Background()
	fakeCredExec(t, "tomb-fd")
	tb := &Tomb{Bin: "tomb", Timeout: teamsTombWait, Cred: NewCred("band-cred", credTeams)}
	msgs, err := tb.Messages(ctx, "19:abc@thread.v2", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "fd-ok" {
		t.Fatalf("msgs = %+v", msgs)
	}
	// Control: without the cred the fake refuses (it found no fd), so the pass above is the fd's doing.
	tb.Cred = nil
	if _, err := tb.Messages(ctx, "19:abc@thread.v2", 5); err == nil || !strings.Contains(err.Error(), "TOMB_AAD_TOKEN_FD=") {
		t.Fatalf("control: expected the fake to refuse without fd, got %v", err)
	}
}

// A shorthand PR (owner/repo#N) took the github.com default, which ebac cannot
// reach since phase 1: three critics failed every round (2026-10-24).
func TestShorthandPRResolvesToTheCredentialedForge(t *testing.T) {
	ref, err := ParsePRRef("bic/aether#64462")
	if err != nil {
		t.Fatal(err)
	}
	host, err := forgeHost([]PRRef{ref}, defaultForgeHost())
	if err != nil || host != "microsoft.ghe.com" {
		t.Fatalf("shorthand resolved to %q (%v); want microsoft.ghe.com", host, err)
	}
	if err := checkForgeHost(host); err != nil {
		t.Fatalf("credentialed host refused: %v", err)
	}
	// Control: an uncredentialed host is refused at creation, naming the fix.
	if err := checkForgeHost("github.com"); err == nil || !strings.Contains(err.Error(), "full PR URL") {
		t.Fatalf("github.com accepted or unhelpful: %v", err)
	}
}
