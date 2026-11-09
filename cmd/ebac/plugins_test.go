package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// The shipped plugin binaries answer the protocol through a real exec: each
// is built and asked for a brief, the one op that needs no upstream.
func TestPluginBinariesSpeakTheProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", dir, "../ebac-gh", "../ebac-keel", "../ebac-tomb")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugins: %v\n%s", err, out)
	}
	t.Setenv("EBAC_PLUGIN_DIR", dir)
	t.Setenv(plugin.SandboxOffEnv, "off") // the protocol, not the sandbox (internal/plugin tests that)
	r, err := plugin.NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	wake := plugin.Wire([]model.Event{{Kind: "x_new", Tier: model.TierWake, PR: "p", Author: "a", Detail: "hello"}})
	for _, c := range []struct {
		critic model.Critic
		want   string
	}{
		{model.Critic{Name: "g", FormID: "@g", Source: "github-pr"}, "ebac g"},
		{model.Critic{Name: "a", FormID: "@a", Source: "github-author", Author: &model.AuthorRef{Login: "someone"}}, "PRs by someone"},
		{model.Critic{Name: "k", FormID: "@k", Source: "keel-threads", Keel: &model.KeelRef{Repo: "notes/x"}}, "answered on keel notes/x"},
		{model.Critic{Name: "t", FormID: "@t", Source: "teams-chat", Chat: &model.ChatRef{ID: "19:x", Topic: "chat"}}, `in "chat"`},
	} {
		name, err := plugin.For(c.critic.SourceName())
		if err != nil {
			t.Fatal(err)
		}
		rep, err := r.Call(context.Background(), name, plugin.Request{Op: plugin.OpBrief, Critic: c.critic, Pending: wake, Me: "me"})
		if err != nil {
			t.Fatalf("%s: %v", c.critic.Source, err)
		}
		if !strings.Contains(rep.Brief, c.want) {
			t.Fatalf("%s brief %q lacks %q", c.critic.Source, rep.Brief, c.want)
		}
	}
}

// ebac-bandhub through a real exec: a round against a fake hub socket returns the mirror's keys, Done, and no events.
func TestBandHubPluginBinaryRound(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", dir, "../ebac-bandhub").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(t.TempDir(), "hub.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stats":
			w.Write([]byte(`{"hub":{"now":1794261929938,"node_stale_s":30},"queues":[{"queue":"gate.change","counts":{"ready":1,"leased":0},"wait_s":{"p50":120}}]}`))
		case "/nodes":
			w.Write([]byte(`[{"node":"leob","age_s":2,"snapshot":{"workers":{"busy":0,"total":2},"disk":{"free_bytes":858993459200}}}]`))
		case "/jobs":
			if r.URL.Query().Get("state") == "ready" {
				w.Write([]byte(`[{"id":7,"queue":"gate.change","state":"ready","created":1794261829938,"payload":{"template":"gate.change","params":{"sha":"abcdef0123456789","label":"x"}}}]`))
				return
			}
			w.Write([]byte(`[]`))
		default:
			http.Error(w, "not for a meta reader", http.StatusForbidden)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	t.Setenv("EBAC_PLUGIN_DIR", dir)
	t.Setenv(plugin.SandboxOffEnv, "off") // the protocol, not the sandbox (internal/plugin tests that)
	r, err := plugin.NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := model.Critic{Name: "band-state", FormID: "@b", Source: model.SourceBandHub, Band: &model.BandRef{Socket: sock, Utility: []string{"leob"}}}
	name, err := plugin.For(c.SourceName())
	if err != nil || name != "bandhub" {
		t.Fatalf("plugin.For(band-hub) = %q, %v", name, err)
	}
	rep, err := r.Call(context.Background(), name, plugin.Request{Op: plugin.OpRound, Critic: c})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || len(rep.Events) != 0 || rep.Error != "" || len(rep.Set) != 4 || strings.Join(rep.Unset, ",") != "live" {
		t.Fatalf("reply: done %v events %d error %q keys %d unset %v", rep.Done, len(rep.Events), rep.Error, len(rep.Set), rep.Unset)
	}
	for k, want := range map[string]string{"queues": "gate.change ready=1 leased=0 oldest_ready=7@", "nodes": "leob class=utility free_slots=2"} {
		if !strings.Contains(string(rep.Set[k]), want) {
			t.Fatalf("%s = %s; want %q", k, rep.Set[k], want)
		}
	}
	// A brief is refused: the mirror never wakes anyone. The plugin's reason is on its stderr (the journal).
	var stderr strings.Builder
	r.Stderr = &stderr
	if _, err := r.Call(context.Background(), name, plugin.Request{Op: plugin.OpBrief, Critic: c}); err == nil || !strings.Contains(stderr.String(), "not supported") {
		t.Fatalf("brief: %v; stderr %q", err, stderr.String())
	}
}

// ebac-roster through a real exec, inside its sandbox when bubblewrap is at
// hand: its whole input arrives on stdin, so a round still derives the roster
// (the positive control for a plugin that is shown no grant at all).
func TestRosterPluginBinaryRound(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", dir, "../ebac-roster").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := &plugin.Runner{Dir: dir, Timeout: plugin.DefaultTimeout, Stderr: os.Stderr}
	if bw := os.Getenv("EBAC_TEST_BWRAP"); bw != "" {
		fig := filepath.Join(t.TempDir(), "figaro")
		if err := os.Mkdir(fig, 0o700); err != nil {
			t.Fatal(err)
		}
		r.Sandbox = &plugin.Sandbox{Bwrap: bw, Grants: plugin.Grants{plugin.GrantFigaro: {filepath.Join(fig, "angelus.sock")}}}
	}
	arias := []model.Aria{
		{ID: "11111111aaaaaaaaaaaaaaaaaaaaaaaa", Handle: "lead-seat", Kind: "conversation", State: "idle"},
		{ID: "@aaaa0001", Kind: "form", Name: "alpha-leader", TargetAria: "11111111"},
	}
	c := model.Critic{Name: "roles", FormID: "@r", Source: model.SourceRoster}
	rep, err := r.Call(context.Background(), "roster", plugin.Request{Op: plugin.OpRound, Critic: c, Arias: arias})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || len(rep.Events) != 0 || !strings.Contains(string(rep.Set["role:alpha-leader"]), "11111111 lead-seat") {
		t.Fatalf("sandboxed=%v reply: done %v events %d set %v", r.Sandbox != nil, rep.Done, len(rep.Events), rep.Set)
	}
	t.Logf("sandboxed=%v role:alpha-leader = %s", r.Sandbox != nil, rep.Set["role:alpha-leader"])
}
