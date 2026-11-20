package pluginrun

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gluck/ebac/sdk/plugin"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
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
		plugin.Serve(func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
			evs := plugin.Unwire(req.Pending)
			for i := range evs {
				evs[i].Text += " (seen)"
			}
			rep := &plugin.Reply{Critic: &req.Critic, Events: plugin.Wire(evs), Brief: string(req.Op)}
			rep.Events[0].Inbox = []string{"k1"}
			return rep, nil
		})
	case "probe":
		plugin.Serve(func(context.Context, plugin.Request) (*plugin.Reply, error) { return &plugin.Reply{Log: probe()}, nil })
	case "fail":
		plugin.Serve(func(context.Context, plugin.Request) (*plugin.Reply, error) {
			return nil, fmt.Errorf("upstream said no")
		})
	case "garbage":
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Print("not json")
	case "unknown-field":
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Printf(`{"protocol":%d,"surprise":true}`, plugin.Version)
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

func pending() []plugin.Event {
	e := model.Event{Kind: "teams_new", Tier: model.TierWake, Comment: "c1", Author: "a"}
	e.Text = "full body"
	return plugin.Wire([]model.Event{e})
}

// The full text of an event and its inbox keys cross the wire both ways:
// model.Event alone drops Text (json:"-").
func TestRunnerRoundTripKeepsTextAndInbox(t *testing.T) {
	r := runner(t, "echo")
	rep, err := r.Call(context.Background(), "fake", plugin.Request{Op: plugin.OpBrief, Critic: model.Critic{Name: "c"}, Pending: pending()})
	if err != nil {
		t.Fatal(err)
	}
	evs := plugin.Unwire(rep.Events)
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
			_, err := r.Call(context.Background(), "fake", plugin.Request{Op: plugin.OpRound})
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
	_, err := r.Call(context.Background(), "fake", plugin.Request{Op: plugin.OpRound})
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestRunnerMissingPlugin(t *testing.T) {
	r := runner(t, "echo")
	_, err := r.Call(context.Background(), "nope", plugin.Request{Op: plugin.OpRound})
	if err == nil || !strings.Contains(err.Error(), "ebac-nope") {
		t.Fatalf("err = %v, want the missing path named", err)
	}
}

// The handler's own error reaches the core with its text (stderr carries it
// to the journal; the call error says the plugin failed).
func TestRunnerHandlerErrorGoesToStderr(t *testing.T) {
	r := runner(t, "fail")
	_, _ = r.Call(context.Background(), "fake", plugin.Request{Op: plugin.OpRound})
	if got := r.Stderr.(*strings.Builder).String(); !strings.Contains(got, "upstream said no") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestInProcMatchesTheWire(t *testing.T) {
	ip := plugin.InProc{"x": func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		return &plugin.Reply{Events: req.Pending}, nil
	}}
	rep, err := ip.Call(context.Background(), "x", plugin.Request{Op: plugin.OpBrief, Pending: pending()})
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Unwire(rep.Events)[0].Text != "full body" {
		t.Fatalf("in-process call lost the text: %+v", rep.Events)
	}
	if _, err := ip.Call(context.Background(), "y", plugin.Request{}); err == nil {
		t.Fatal("an unknown in-process plugin answered")
	}
}

func TestForNamesEverySource(t *testing.T) {
	for src, want := range map[string]string{"": "gh", "github-pr": "gh", "github-author": "gh", "keel-threads": "keel", "teams-chat": "tomb", "github-cache": "gh"} {
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
	plugin.Adopt(core, from)
	if core.Name != "c" || core.FormID != "@f" || core.Write || len(core.Runtime.PendingEvents) != 1 || core.Runtime.OutboxWatermark != 7 {
		t.Fatalf("a plugin rewrote a core field: %+v", core)
	}
	if len(core.PRs) != 1 || core.Runtime.LastError != "e" || !core.Runtime.Stopped {
		t.Fatalf("a source field was not adopted: %+v", core)
	}
}

// The GitHub cache's state (its entities' marks and due times) is the plugin's: adopted whole, or every round would
// re-read every PR in full. RED if Adopt drops Runtime.GHCache.
func TestAdoptKeepsTheGHCacheState(t *testing.T) {
	from := &model.Critic{}
	from.Runtime.GHCache = json.RawMessage(`{"entities":{"aether-pr-1":{"head":"ab"}}}`)
	core := &model.Critic{}
	plugin.Adopt(core, from)
	if got := string(core.Runtime.GHCache); got != `{"entities":{"aether-pr-1":{"head":"ab"}}}` {
		t.Fatalf("gh_cache state not adopted: %q", got)
	}
}

// A review critic's probe fingerprints and its last full read are the github source's: not adopted, every round
// read every PR in full (ledger-front 11-17: 14 points a round instead of 1).
func TestAdoptKeepsTheProbe(t *testing.T) {
	at := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	from := &model.Critic{}
	from.Runtime.Probe, from.Runtime.FullReadAt = map[string]string{"bic/aether#1": "ab"}, &at
	core := &model.Critic{}
	plugin.Adopt(core, from)
	if core.Runtime.Probe["bic/aether#1"] != "ab" || core.Runtime.FullReadAt == nil || !core.Runtime.FullReadAt.Equal(at) {
		t.Fatalf("probe state not adopted: %+v %v", core.Runtime.Probe, core.Runtime.FullReadAt)
	}
}

// org-approve runs in the approve plugin, and adopts its scores and asks
// (the core keeps its configuration).
func TestOrgApprovePluginAndAdopt(t *testing.T) {
	if got, err := For(model.SourceOrgApprove); err != nil || got != "approve" {
		t.Fatalf("For(org-approve) = %q, %v", got, err)
	}
	core := &model.Critic{Name: "oa", OrgApprove: &model.OrgApproveWatch{Approver: "jack"}}
	from := &model.Critic{Name: "oa", OrgApprove: &model.OrgApproveWatch{Approver: "flake:x"}}
	from.Runtime.OrgApprove = &model.OrgApproveState{Asks: map[string]*model.OrgAsk{"oa-1-aaaaaaa": {PR: 1}}}
	plugin.Adopt(core, from)
	if core.Runtime.OrgApprove == nil || core.Runtime.OrgApprove.Asks["oa-1-aaaaaaa"] == nil || core.OrgApprove.Approver != "jack" {
		t.Fatalf("adopt: %+v", core)
	}
}
