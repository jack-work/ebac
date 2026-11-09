package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// The test binary doubles as a plugin: run as ebac-fake with
// EBAC_FAKE_PLUGIN set, it behaves as that mode says.
func TestMain(m *testing.M) {
	if mode := os.Getenv("EBAC_FAKE_PLUGIN"); mode != "" {
		fakePlugin(mode)
		return
	}
	os.Exit(m.Run())
}

func fakePlugin(mode string) {
	switch mode {
	case "echo":
		Serve(func(_ context.Context, req Request) (*Reply, error) {
			evs := Unwire(req.Pending)
			for i := range evs {
				evs[i].Text += " (seen)"
			}
			rep := &Reply{Critic: &req.Critic, Events: Wire(evs), Brief: string(req.Op)}
			rep.Events[0].Inbox = []string{"k1"}
			return rep, nil
		})
	case "probe":
		Serve(func(context.Context, Request) (*Reply, error) { return &Reply{Log: probe()}, nil })
	case "fail":
		Serve(func(context.Context, Request) (*Reply, error) { return nil, fmt.Errorf("upstream said no") })
	case "garbage":
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Print("not json")
	case "unknown-field":
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Printf(`{"protocol":%d,"surprise":true}`, Version)
	case "version":
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"protocol":99}`)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "huge":
		_, _ = io.Copy(io.Discard, os.Stdin)
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 10; i++ {
			fmt.Print(chunk)
		}
	}
	os.Exit(0)
}

func runner(t *testing.T, mode string) *Runner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "ebac-fake")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EBAC_FAKE_PLUGIN", mode)
	var stderr strings.Builder
	return &Runner{Dir: dir, Timeout: 5 * time.Second, Stderr: &stderr}
}

func pending() []Event {
	e := model.Event{Kind: "teams_new", Tier: model.TierWake, Comment: "c1", Author: "a"}
	e.Text = "full body"
	return Wire([]model.Event{e})
}

// The full text of an event and its inbox keys cross the wire both ways:
// model.Event alone drops Text (json:"-").
func TestRunnerRoundTripKeepsTextAndInbox(t *testing.T) {
	r := runner(t, "echo")
	rep, err := r.Call(context.Background(), "fake", Request{Op: OpBrief, Critic: model.Critic{Name: "c"}, Pending: pending()})
	if err != nil {
		t.Fatal(err)
	}
	evs := Unwire(rep.Events)
	if len(evs) != 1 || evs[0].Text != "full body (seen)" {
		t.Fatalf("event text lost on the wire: %+v", rep.Events)
	}
	if got := rep.Events[0].Inbox; len(got) != 1 || got[0] != "k1" {
		t.Fatalf("inbox keys lost: %v", got)
	}
	if rep.Critic == nil || rep.Critic.Name != "c" || rep.Brief != "brief" {
		t.Fatalf("reply: %+v", rep)
	}

	// Saw it fail: plain model.Event really does drop the text, which is why
	// the wire type exists.
	b, _ := json.Marshal(evs[0])
	if strings.Contains(string(b), "full body") {
		t.Fatalf("model.Event now serialises Text; the wire wrapper may be redundant: %s", b)
	}
}

// Every way a plugin can misbehave is a failed call that names the plugin, the
// op and the cause, never a hang or a crash.
func TestRunnerFailures(t *testing.T) {
	for _, c := range []struct{ mode, want string }{
		{"fail", "plugin fake round: exit status 1"},
		{"garbage", "bad reply"},
		{"unknown-field", `unknown field "surprise"`},
		{"version", "protocol 99, want 1"},
		{"huge", "reply over"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			r := runner(t, c.mode)
			_, err := r.Call(context.Background(), "fake", Request{Op: OpRound})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestRunnerTimeout(t *testing.T) {
	r := runner(t, "sleep")
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.Call(context.Background(), "fake", Request{Op: OpRound})
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestRunnerMissingPlugin(t *testing.T) {
	r := runner(t, "echo")
	_, err := r.Call(context.Background(), "nope", Request{Op: OpRound})
	if err == nil || !strings.Contains(err.Error(), "ebac-nope") {
		t.Fatalf("err = %v, want the missing path named", err)
	}
}

// The handler's own error reaches the core with its text (stderr carries it
// to the journal; the call error says the plugin failed).
func TestRunnerHandlerErrorGoesToStderr(t *testing.T) {
	r := runner(t, "fail")
	_, _ = r.Call(context.Background(), "fake", Request{Op: OpRound})
	if got := r.Stderr.(*strings.Builder).String(); !strings.Contains(got, "upstream said no") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestInProcMatchesTheWire(t *testing.T) {
	ip := InProc{"x": func(_ context.Context, req Request) (*Reply, error) {
		return &Reply{Events: req.Pending}, nil
	}}
	rep, err := ip.Call(context.Background(), "x", Request{Op: OpBrief, Pending: pending()})
	if err != nil {
		t.Fatal(err)
	}
	if Unwire(rep.Events)[0].Text != "full body" {
		t.Fatalf("in-process call lost the text: %+v", rep.Events)
	}
	if _, err := ip.Call(context.Background(), "y", Request{}); err == nil {
		t.Fatal("an unknown in-process plugin answered")
	}
}

func TestForNamesEverySource(t *testing.T) {
	for src, want := range map[string]string{"": "gh", "github-pr": "gh", "github-author": "gh", "keel-threads": "keel", "teams-chat": "tomb"} {
		if got, err := For(src); err != nil || got != want {
			t.Fatalf("For(%q) = %q, %v; want %q", src, got, err, want)
		}
	}
	if _, err := For("ado-pipelines"); err == nil {
		t.Fatal("an unknown source got a plugin")
	}
}

func TestAdoptKeepsTheCoresFields(t *testing.T) {
	core := &model.Critic{Name: "c", FormID: "@f", Write: false}
	core.Runtime.PendingEvents = []model.Event{{Kind: "k"}}
	core.Runtime.OutboxWatermark = 7
	from := &model.Critic{Name: "evil", FormID: "@other", Write: true, PRs: []model.PRRef{{Owner: "o", Repo: "r", Number: 1}}}
	from.Runtime.LastError = "e"
	from.Runtime.Stopped = true
	Adopt(core, from)
	if core.Name != "c" || core.FormID != "@f" || core.Write || len(core.Runtime.PendingEvents) != 1 || core.Runtime.OutboxWatermark != 7 {
		t.Fatalf("a plugin rewrote a core field: %+v", core)
	}
	if len(core.PRs) != 1 || core.Runtime.LastError != "e" || !core.Runtime.Stopped {
		t.Fatalf("a source field was not adopted: %+v", core)
	}
}
