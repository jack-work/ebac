package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Trusted is what only ebac's own configuration (the system flake renders
// /etc/ebac/config.json; key "trusted") may set: where a critic's forge host,
// keel URL and token command, clone, worktree root and notes point. A
// critic's FORM is writable by its seats and studiers, so the values stored
// there are ignored and flagged (review d9d993be F5, the Thinker's #10): a
// form token_cmd is never executed, a form host never chooses the credential.
type Trusted struct {
	// ForgeHosts are the hosts a critic may name; the first is the default.
	ForgeHosts []string `json:"forge_hosts"`
	// Critics are the per-critic trusted values, by critic name.
	Critics map[string]TrustedCritic `json:"critics,omitempty"`
}

// TrustedCritic is one critic's trusted values (all optional).
type TrustedCritic struct {
	KeelURL      string   `json:"keel_url,omitempty"`
	TokenCmd     []string `json:"token_cmd,omitempty"`
	Clone        string   `json:"clone,omitempty"`
	WorktreeRoot string   `json:"worktree_root,omitempty"`
	Notes        string   `json:"notes,omitempty"`
}

// DefaultTrusted: the one forge host ebac reads today, and nothing per critic.
func DefaultTrusted() Trusted { return Trusted{ForgeHosts: []string{"microsoft.ghe.com"}} }

// TrustedConfigPath is ebac's configuration file.
const TrustedConfigPath = "/etc/ebac/config.json"

// LoadTrusted reads the "trusted" key of path; a missing file or key is the
// defaults, an unreadable file an error (callers fail closed on it).
func LoadTrusted(path string) (Trusted, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultTrusted(), nil
	}
	if err != nil {
		return Trusted{}, err
	}
	var f struct {
		Trusted *Trusted `json:"trusted"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return Trusted{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Trusted == nil {
		return DefaultTrusted(), nil
	}
	if len(f.Trusted.ForgeHosts) == 0 {
		f.Trusted.ForgeHosts = DefaultTrusted().ForgeHosts
	}
	return *f.Trusted, nil
}

// Apply replaces w's trusted fields with t's and returns the names of the
// fields whose FORM value was set and differs (ignored; ebac ls flags them).
// A form host on the allowlist is kept; any other is replaced by the default.
func (t Trusted) Apply(w *Critic) []string {
	var ignored []string
	note := func(name string, formSet bool) {
		if formSet {
			ignored = append(ignored, name)
		}
	}
	if w.Host != "" && !slices.Contains(t.ForgeHosts, w.Host) {
		note("host", true)
		w.Host = ""
	}
	if w.Host == "" && len(t.ForgeHosts) > 0 && !nonForge(w) {
		w.Host = t.ForgeHosts[0]
	}
	tc := t.Critics[w.Name]
	if k := w.Keel; k != nil {
		note("keel.url", k.URL != "" && k.URL != tc.KeelURL)
		note("keel.token_cmd", len(k.TokenCmd) > 0 && !slices.Equal(k.TokenCmd, tc.TokenCmd))
		k.URL, k.TokenCmd = tc.KeelURL, slices.Clone(tc.TokenCmd)
	}
	if b := w.Branches; b != nil {
		note("keel_branches.url", b.URL != "" && b.URL != tc.KeelURL)
		note("keel_branches.token_cmd", len(b.TokenCmd) > 0 && !slices.Equal(b.TokenCmd, tc.TokenCmd))
		b.URL, b.TokenCmd = tc.KeelURL, slices.Clone(tc.TokenCmd)
	}
	if wt := w.Worktree; wt != nil {
		note("worktree.clone", wt.Clone != "" && wt.Clone != tc.Clone)
		note("worktree.root", wt.Root != "" && wt.Root != tc.WorktreeRoot)
	}
	w.Worktree = nil
	if tc.Clone != "" || tc.WorktreeRoot != "" {
		w.Worktree = &Worktree{Clone: tc.Clone, Root: tc.WorktreeRoot}
	}
	note("notes", w.Notes != "" && w.Notes != tc.Notes)
	w.Notes = tc.Notes
	note("figaro_socket", w.FigaroSock != "")
	w.FigaroSock = ""
	return ignored
}

// nonForge: sources that read no forge (their host stays empty).
func nonForge(w *Critic) bool {
	switch w.SourceName() {
	case "teams-chat", "keel-threads", SourceKeelBranches, SourceRoster:
		return true
	}
	return false
}

// IgnoredNote is the ebac ls flag for ignored form fields.
func IgnoredNote(fields []string) string {
	return "form config ignored: " + strings.Join(fields, ",")
}
