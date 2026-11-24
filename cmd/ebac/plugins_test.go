package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/livetest"
	"github.com/gluck/ebac/internal/pluginrun"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
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
	// Every plugin is its own module, built and tested in its own derivation (pkgs/ebac.nix mkEbacPlugin); inside the
	// core's nix build their module graphs are not available, so this cross-module exec runs only in a dev tree.
	if os.Getenv("NIX_BUILD_TOP") != "" {
		t.Skip("plugin modules are built in their own derivations")
	}
	dir := t.TempDir()
	builds := [][]string{
		{"-C", "../../plugins/github", "-o", dir, "./cmd/ebac-gh"},
		{"-C", "../../plugins/teams", "-o", dir, "./cmd/ebac-tomb"},
		{"-C", "../../plugins/keel", "-o", dir, "./cmd/ebac-keel"},
	}
	for _, b := range builds {
		if out, err := exec.Command("go", append([]string{"build"}, b...)...).CombinedOutput(); err != nil {
			t.Fatalf("build plugins %v: %v\n%s", b, err, out)
		}
	}
	t.Setenv("EBAC_PLUGIN_DIR", pinned(t, dir))
	t.Setenv(pluginrun.SandboxOffEnv, "off") // the protocol, not the sandbox (internal/pluginrun tests that)
	r, err := pluginrun.NewRunner(nil)
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
		name, err := pluginrun.For(c.critic.SourceName())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "ebac-"+name)); err != nil {
			t.Fatalf("%s: plugin binary not built: %v", name, err)
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
	dir := buildBandHub(t)
	sock := fakeBandHub(t)

	t.Setenv("EBAC_PLUGIN_DIR", pinned(t, dir))
	t.Setenv(pluginrun.SandboxOffEnv, "off") // the protocol, not the sandbox (internal/pluginrun tests that)
	r, err := pluginrun.NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := model.Critic{Name: "band-state", FormID: "@b", Source: model.SourceBandHub, Band: &model.BandRef{Socket: sock, Utility: []string{"leob"}}}
	name, err := pluginrun.For(c.SourceName())
	if err != nil || name != "bandhub" {
		t.Fatalf("pluginrun.For(band-hub) = %q, %v", name, err)
	}
	rep, err := r.Call(context.Background(), name, plugin.Request{Op: plugin.OpRound, Critic: c})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || len(rep.Events) != 0 || rep.Error != "" || len(rep.Set) != 5 || strings.Join(rep.Unset, ",") != "live" {
		t.Fatalf("reply: done %v events %d error %q keys %d unset %v", rep.Done, len(rep.Events), rep.Error, len(rep.Set), rep.Unset)
	}
	for k, want := range map[string]string{"queues": `"gate_change":"gate.change ready=1 leased=0 oldest_ready=7@`, "nodes": `"leob":"leob class=utility`, "node_load": `"leob":"leob free_slots=2`} {
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
	if os.Getenv("NIX_BUILD_TOP") != "" {
		// A plugin module builds in its own derivation (pkgs/ebac.nix mkEbacPlugin); inside the core's nix
		// build its module graph is not available, so the cross-module build runs only in a dev tree.
		t.Skip("ebac-roster is a plugin module: tested in its own derivation")
	}
	dir := t.TempDir()
	// The roster is its own module (plugins/roster): build it from there.
	build := exec.Command("go", "build", "-o", dir, "./cmd/ebac-roster")
	build.Dir = filepath.Join("..", "..", "plugins", "roster")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := &pluginrun.Runner{Dir: dir, Timeout: pluginrun.DefaultTimeout, Stderr: os.Stderr}
	if bw := os.Getenv("EBAC_TEST_BWRAP"); bw != "" {
		fig := filepath.Join(t.TempDir(), "figaro")
		if err := os.Mkdir(fig, 0o700); err != nil {
			t.Fatal(err)
		}
		r.Sandbox = &pluginrun.Sandbox{Bwrap: bw, Grants: pluginrun.Grants{pluginrun.GrantFigaro: {filepath.Join(fig, "angelus.sock")}}}
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

// buildBandHub builds the band-hub plugin from its own module (plugins/bandhub):
// the core never imports it, it only execs the binary.
func buildBandHub(t *testing.T) string {
	t.Helper()
	if os.Getenv("NIX_BUILD_TOP") != "" || strings.Contains(os.Getenv("GOFLAGS"), "-mod=vendor") {
		t.Skip("SKIP bandhub-binary: inside a nix build (offline, vendored) the core cannot build plugins/bandhub, a separate module; the plugin's own derivation runs its tests")
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", dir, "./cmd/ebac-bandhub")
	build.Dir = filepath.Join("..", "..", "plugins", "bandhub")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugins/bandhub: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ebac-bandhub")); err != nil {
		t.Fatalf("plugins/bandhub built no ebac-bandhub: %v", err)
	}
	return dir
}

// fakeBandHub serves a minimal hub (one ready job, one node) on a unix socket,
// the metadata endpoints only, and returns the socket's path.
func fakeBandHub(t *testing.T) string {
	t.Helper()
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
	return sock
}

// Live: the core polls a band-hub mirror on a real role form. The plugin is a
// replay of its recorded replies (testdata/bandhub-golden-round.json, the same
// bytes plugins/bandhub pins in its golden test), so the core's side of the
// round is tested without a hub or a forge: the mirror's keys land, a key an
// earlier version wrote is dropped, and the second (quiet) round is reported
// unchanged and rewrites none of the mirror's keys. Moved here from the plugin
// when the plugin left the core module: the core may exec it, never import it.
func TestLiveBandHubMirrorThroughTheCore(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	var recorded []*plugin.Reply
	b, err := os.ReadFile(filepath.Join("testdata", "bandhub-golden-round.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &recorded); err != nil || len(recorded) != 2 || len(recorded[0].Set) != 5 {
		t.Fatalf("recorded replies: %d, err %v", len(recorded), err)
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-bandstate")
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, f, id)
	w := model.Critic{Name: "livetest-bandstate-" + strings.TrimPrefix(id, "@")[:8], FormID: id, CreatedAt: time.Now().UTC(),
		Source: model.SourceBandHub, Band: &model.BandRef{Socket: model.DefaultBandHubSocket, Utility: []string{"leob", "ross"}}}
	if err := st.SaveCriticRecordingForTest(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedTrigger(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, id, "live", []string{"4246 old line"}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := 0
	pollOnce := func() {
		t.Helper()
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: replay{recorded: recorded, round: &round}}
		if err := p.Poll(ctx, w.Name); err != nil {
			t.Fatalf("poll: %v\n%s", err, out.String())
		}
	}
	mirror := []string{"queues", "nodes", "node_load", "dupes", "updated"}
	pollOnce()
	cf, err := st.ReadForm(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mirror {
		if _, ok := cf.Keys[k]; !ok {
			t.Fatalf("key %s missing after round 1\n%s", k, out.String())
		}
	}
	if _, ok := cf.Keys["live"]; ok {
		t.Fatalf("the dropped key live survived round 1: %s", cf.Keys["live"])
	}
	c, err := st.LoadCritic(ctx, w.Name)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.LastError != "" || !sameJSON(c.Runtime.BandHub, recorded[0].Critic.Runtime.BandHub) {
		t.Fatalf("after round 1 the core did not save the plugin's state: error %q, state %d bytes (want %d)",
			c.Runtime.LastError, len(c.Runtime.BandHub), len(recorded[0].Critic.Runtime.BandHub))
	}
	pollOnce()
	cf2, err := st.ReadForm(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mirror {
		if string(cf2.Keys[k]) != string(cf.Keys[k]) {
			t.Fatalf("a quiet round rewrote %s", k)
		}
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Fatalf("second round not reported unchanged:\n%s", out.String())
	}
}

// replay answers band-hub rounds with recorded replies, in order, carrying the
// core's critic (name, form) and the recorded plugin state.
type replay struct {
	recorded []*plugin.Reply
	round    *int
}

func (r replay) Call(_ context.Context, name string, req plugin.Request) (*plugin.Reply, error) {
	if name != "bandhub" || req.Op != plugin.OpRound || *r.round >= len(r.recorded) {
		return nil, fmt.Errorf("replay: unexpected call %s %s (round %d)", name, req.Op, *r.round)
	}
	if *r.round > 0 && !sameJSON(req.Critic.Runtime.BandHub, r.recorded[*r.round-1].Critic.Runtime.BandHub) {
		return nil, fmt.Errorf("replay: round %d got %d bytes of plugin state, not the %d the previous round returned",
			*r.round+1, len(req.Critic.Runtime.BandHub), len(r.recorded[*r.round-1].Critic.Runtime.BandHub))
	}
	rec := *r.recorded[*r.round]
	*r.round++
	c := req.Critic
	c.Runtime.BandHub = rec.Critic.Runtime.BandHub
	c.Runtime.LastSuccess = rec.Critic.Runtime.LastSuccess
	rec.Critic = &c
	return &rec, nil
}

// sameJSON compares two JSON values as values (the golden is indented and in
// the plugin's key order; the core stores it compact and re-encoded). Empty is
// never the same as non-empty.
func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// pluginSources are what each built plugin says in its hello (its main's Info).
var pluginSources = map[string][]string{
	"gh":      {"github-pr", "github-author", model.SourceReleases, model.SourceScout, model.SourceGHCache},
	"tomb":    {"teams-chat"},
	"keel":    {"keel-threads", model.SourceKeelBranches},
	"bandhub": {model.SourceBandHub},
}

// pinned lays the plugins built into dir out as the fleet ships them: bin/ebac-<name>
// plus a manifest pinning its sha256, in a dir the test treats as the store.
func pinned(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := pluginrun.StoreRoot
	pluginrun.StoreRoot = filepath.Dir(real) + "/"
	t.Cleanup(func() { pluginrun.StoreRoot = old })
	if err := os.MkdirAll(filepath.Join(real, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exes, _ := filepath.Glob(filepath.Join(real, "ebac-*"))
	for _, exe := range exes {
		name := strings.TrimPrefix(filepath.Base(exe), "ebac-")
		if err := os.Rename(exe, filepath.Join(real, "bin", filepath.Base(exe))); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(real, "bin", filepath.Base(exe)))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(body)
		m, _ := json.Marshal(map[string]any{"name": name, "exe": "bin/ebac-" + name, "sha256": hex.EncodeToString(h[:]),
			"protocol": map[string]int{"major": plugin.Version, "minor": plugin.Minor}, "sources": pluginSources[name]})
		if err := os.WriteFile(filepath.Join(real, name+".plugin.json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return real
}
