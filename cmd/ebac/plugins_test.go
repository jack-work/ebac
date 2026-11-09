package main

import (
	"context"
	"os/exec"
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
	r, err := plugin.NewRunner()
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
