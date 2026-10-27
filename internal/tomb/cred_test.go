package tomb

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/forge"
)

func TestTombGetsItsTokenOnFd3(t *testing.T) {
	ctx := context.Background()
	fakeCredExec(t, "tomb-fd")
	tb := &Tomb{Bin: "tomb", Timeout: TeamsTombWait, Cred: forge.NewCred("band-cred", forge.CredTeams)}
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

// fakeCredExec routes every child (band-cred, gh, tomb) to TestCredHelperProcess.
// The first child arg after "--" is the scenario; the rest is the real argv.
func fakeCredExec(t *testing.T, scenario string) {
	t.Helper()
	orig := forge.ExecCommandContext
	forge.ExecCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestCredHelperProcess", "--", scenario, name}, args...)...)
	}
	t.Cleanup(func() { forge.ExecCommandContext = orig })
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
		case argv[1] == forge.CredGitHub || argv[1] == forge.CredTeams:
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

const fakeTok = "gho_FAKE0123456789abcdef"
