package tomb

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge"
)

func TestTombErrorClassification(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		scenario string
		gone     bool
		text     string
	}{
		{"gone404", true, "chat API returned 404"},
		{"gone403", true, "chat API returned 403"},
		{"auth403", false, "auth service returned 403"},
		{"down", false, "connection refused"},
		{"garbage", false, "unreadable answer"},
	} {
		_, err := fakeTomb(t, c.scenario).Messages(ctx, "19:abc@thread.v2", 5)
		if err == nil {
			t.Fatalf("%s: no error", c.scenario)
		}
		if errors.Is(err, ErrTombGone) != c.gone || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: gone=%v err=%v; want gone=%v containing %q", c.scenario, errors.Is(err, ErrTombGone), err, c.gone, c.text)
		}
	}
	// Positive control: the fake answers when it should.
	msgs, err := fakeTomb(t, "ok").Messages(ctx, "19:abc@thread.v2", 5)
	if err != nil || len(msgs) != 1 || msgs[0].From != "A" {
		t.Fatalf("ok scenario: %v %+v", err, msgs)
	}
}

// ---------- tomb: fake exec ----------

func fakeTomb(t *testing.T, scenario string) *Tomb {
	t.Helper()
	orig := forge.ExecCommandContext
	forge.ExecCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestTombHelperProcess", "--", scenario}, args...)...)
	}
	t.Cleanup(func() { forge.ExecCommandContext = orig })
	return &Tomb{Bin: "unused", Timeout: 10 * time.Second}
}

// TestTombHelperProcess is the fake tomb; gated on "--" in argv like TestHelperProcess.
func TestTombHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		return
	}
	scenario := args[1]
	switch scenario {
	case "ok":
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"<p>x</p>"}]}`)
		os.Exit(0)
	case "ok2":
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"<p>x</p>"},` +
			`{"id":"2","messagetype":"RichText/Html","imdisplayname":"Akshay","content":"<p>please look</p>"}]}`)
		os.Exit(0)
	case "gone404":
		os.Stderr.WriteString("Error: chat API returned 404\n")
	case "gone403":
		os.Stderr.WriteString("Error: chat API returned 403\n")
	case "auth403":
		os.Stderr.WriteString("Error: auth service returned 403: {\"error\":\"interaction_required\"}\n")
	case "down":
		os.Stderr.WriteString("Error: dial unix /run/user/1000/tomb.sock: connect: connection refused\n")
	case "garbage":
		os.Stdout.WriteString("not json")
		os.Exit(0)
	}
	os.Exit(1)
}
