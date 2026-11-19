package model

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// One row per trusted field: a form value is ignored and named; the trusted value (or none) is what the critic gets;
// a form value equal to the trusted one is not flagged. RED if any field reads from the form again.
func TestTrustedApplyIgnoresFormValues(t *testing.T) {
	tr := Trusted{ForgeHosts: []string{"microsoft.ghe.com", "github.com"}, Critics: map[string]TrustedCritic{
		"keel-ok": {KeelURL: "https://keel.example", TokenCmd: []string{"hush", "oauth", "get", "ebac"}},
		"rev-ok":  {Clone: "/srv/clone", WorktreeRoot: "/srv/wt", Notes: "AGENTS.md"},
	}}
	cases := []struct {
		name    string
		w       Critic
		ignored string
		check   func(Critic) bool
	}{
		{"form token_cmd and url ignored, none trusted", Critic{Name: "keel-x", Source: "keel-threads", Keel: &KeelRef{URL: "https://evil", TokenCmd: []string{"touch", "/tmp/pwned"}}},
			"keel.url,keel.token_cmd", func(w Critic) bool { return w.Keel.URL == "" && len(w.Keel.TokenCmd) == 0 }},
		{"trusted keel values replace the form's", Critic{Name: "keel-ok", Source: "keel-threads", Keel: &KeelRef{URL: "https://evil", TokenCmd: []string{"sh"}}},
			"keel.url,keel.token_cmd", func(w Critic) bool {
				return w.Keel.URL == "https://keel.example" && slices.Equal(w.Keel.TokenCmd, []string{"hush", "oauth", "get", "ebac"})
			}},
		{"form equal to trusted is not flagged", Critic{Name: "keel-ok", Source: "keel-threads", Keel: &KeelRef{URL: "https://keel.example", TokenCmd: []string{"hush", "oauth", "get", "ebac"}}},
			"", func(w Critic) bool { return w.Keel.URL == "https://keel.example" }},
		{"keel branches token_cmd ignored", Critic{Name: "aether-work", Source: SourceKeelBranches, Branches: &KeelBranches{URL: "https://evil", TokenCmd: []string{"sh", "-c", "x"}}},
			"keel_branches.url,keel_branches.token_cmd", func(w Critic) bool { return w.Branches.URL == "" && len(w.Branches.TokenCmd) == 0 }},
		{"host off the allowlist replaced by the default", Critic{Name: "a", Source: "github-author", Host: "evil.example"},
			"host", func(w Critic) bool { return w.Host == "microsoft.ghe.com" }},
		{"host on the allowlist kept", Critic{Name: "a", Source: "github-author", Host: "github.com"},
			"", func(w Critic) bool { return w.Host == "github.com" }},
		{"empty host gets the default", Critic{Name: "a", Source: "github-author"},
			"", func(w Critic) bool { return w.Host == "microsoft.ghe.com" }},
		{"a non-forge critic gets no host", Critic{Name: "t", Source: "teams-chat"},
			"", func(w Critic) bool { return w.Host == "" }},
		{"form clone/root/notes ignored, none trusted", Critic{Name: "rev-x", Worktree: &Worktree{Clone: "/tmp/c", Root: "/tmp/r"}, Notes: "/etc/shadow"},
			"worktree.clone,worktree.root,notes", func(w Critic) bool { return w.Worktree == nil && w.Notes == "" }},
		{"trusted clone/root/notes used", Critic{Name: "rev-ok", Worktree: &Worktree{Clone: "/tmp/c"}},
			"worktree.clone", func(w Critic) bool {
				return w.Worktree != nil && w.Worktree.Clone == "/srv/clone" && w.Worktree.Root == "/srv/wt" && w.Notes == "AGENTS.md"
			}},
		{"figaro_socket ignored", Critic{Name: "r", Source: SourceReleases, FigaroSock: "/tmp/sock"},
			"figaro_socket", func(w Critic) bool { return w.FigaroSock == "" }},
	}
	for _, c := range cases {
		w := c.w
		got := strings.Join(tr.Apply(&w), ",")
		if got != c.ignored || !c.check(w) {
			t.Errorf("%s: ignored %q (want %q), critic %+v", c.name, got, c.ignored, w)
		}
	}
}

func TestLoadTrusted(t *testing.T) {
	dir := t.TempDir()
	if tr, err := LoadTrusted(filepath.Join(dir, "absent.json")); err != nil || !slices.Equal(tr.ForgeHosts, []string{"microsoft.ghe.com"}) || len(tr.Critics) != 0 {
		t.Fatalf("absent: %+v %v", tr, err)
	}
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"budget":{},"trusted":{"forge_hosts":["h1"],"critics":{"k":{"keel_url":"https://k"}}}}`), 0o600)
	if tr, err := LoadTrusted(p); err != nil || tr.ForgeHosts[0] != "h1" || tr.Critics["k"].KeelURL != "https://k" {
		t.Fatalf("file: %+v %v", tr, err)
	}
	_ = os.WriteFile(p, []byte(`{"trusted":`), 0o600)
	if _, err := LoadTrusted(p); err == nil {
		t.Fatal("a torn config loaded")
	}
}
