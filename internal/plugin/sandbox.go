package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// BwrapPath is bubblewrap's absolute path, fixed at build time:
//
//	-ldflags "-X github.com/gluck/ebac/internal/plugin.BwrapPath=<store>/bin/bwrap"
//
// It is never looked up on PATH: whoever controls PATH would choose the
// sandbox.
var BwrapPath = ""

// SandboxOffEnv, set to "off", runs plugins unsandboxed. It exists for
// development builds without bubblewrap; a deployed unit never sets it.
const SandboxOffEnv = "EBAC_PLUGIN_SANDBOX"

// Grant is one of the core uid's access grants a plugin might use: a socket
// (or directory) that uid may reach and nothing else on the box can stand in
// for. Everything else a plugin could read is already bounded by Unix
// permissions and the unit's own hardening; these are the powers.
type Grant string

const (
	// GrantFigaro is the angelus socket. No plugin ever sees it.
	GrantFigaro Grant = "figaro"
	// GrantBandCred is band-cred's principal socket: GHE and Teams tokens.
	GrantBandCred Grant = "band-cred"
	// GrantCredCache is the runtime-dir cache of minted tokens.
	GrantCredCache Grant = "cred-cache"
	// GrantKeelDoor is the keel door: read-only keel threads.
	GrantKeelDoor Grant = "keel-door"
	// GrantBandHub is the band hub's metadata reader socket.
	GrantBandHub Grant = "band-hub"
)

// Needs is what one plugin may use for one critic. The network is shared by
// every plugin (each reaches its upstream through the host's egress rules).
func Needs(plugin string, c *model.Critic) []Grant {
	if c != nil && c.SourceName() == model.SourceReleases {
		return nil // public github.com releases, read without a credential
	}
	if c != nil && c.SourceName() == model.SourceScout {
		// GHE, and the hub's metadata for the band jobs on each PR head.
		return []Grant{GrantBandCred, GrantCredCache, GrantBandHub}
	}
	switch plugin {
	case "gh", "tomb":
		return []Grant{GrantBandCred, GrantCredCache}
	case "approve":
		// GHE (ghe-ebac) and Teams (teams-ebac), both from band-cred; the
		// grants file it reads is world-readable band-grants state.
		return []Grant{GrantBandCred, GrantCredCache}
	case "keel":
		return []Grant{GrantKeelDoor}
	case "bandhub":
		// The hub, and GHE for the superseded check's branch tips.
		return []Grant{GrantBandHub, GrantBandCred, GrantCredCache}
	case "roster":
		// Its whole input arrives on stdin (Request.Arias): no grant at all.
		return nil
	}
	return nil
}

// Grants maps each grant to the paths that carry it on this box. A path is
// a socket (its directory is hidden) or a directory (hidden itself).
type Grants map[Grant][]string

// Sandbox runs each plugin under bubblewrap in its own user, pid and mount
// namespaces. The plugin sees the host filesystem except the grants it does
// not need: each of those is an empty tmpfs in its mount namespace, so for
// it the socket does not exist. The figaro grant is always hidden. It keeps
// no capabilities (bwrap drops them), so it cannot unmount a tmpfs; it may
// not create a nested user namespace (--disable-userns), so it cannot win
// them back; its own /proc (--unshare-pid) hides /proc/<core pid>/root, the
// other way to a hidden socket.
type Sandbox struct {
	Bwrap  string
	Grants Grants
}

// Hide is the directories hidden from plugin for critic c: every grant it
// does not need, except a directory that also holds one it does.
func (s *Sandbox) Hide(plugin string, c *model.Critic) []string {
	need := map[Grant]bool{}
	for _, g := range Needs(plugin, c) {
		need[g] = true
	}
	needed := []string{}
	for g, paths := range s.Grants {
		if need[g] && g != GrantFigaro {
			needed = append(needed, paths...)
		}
	}
	if plugin == "bandhub" && c != nil && c.Band != nil && c.Band.Socket != "" {
		needed = append(needed, c.Band.Socket)
	}
	seen := map[string]bool{}
	var hide []string
	for g, paths := range s.Grants {
		if need[g] && g != GrantFigaro {
			continue
		}
		for _, p := range paths {
			d := hideDir(p)
			// Figaro is hidden even over a needed path (a misconfiguration
			// that then fails loudly); any other grant yields to a needed one.
			if d == "" || seen[d] || (g != GrantFigaro && holdsAny(d, needed)) {
				continue
			}
			seen[d] = true
			hide = append(hide, d)
		}
	}
	sort.Strings(hide)
	return hide
}

// hideDir is the directory to cover for a grant path: the path itself if it
// is a directory, else its parent; "" if it does not exist (nothing to hide)
// or is a filesystem root.
func hideDir(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	d := p
	if !fi.IsDir() {
		d = filepath.Dir(p)
	}
	if d == "/" || d == "." {
		return ""
	}
	return d
}

func holdsAny(dir string, paths []string) bool {
	for _, p := range paths {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// Args is bwrap's argument list for one plugin executable.
func (s *Sandbox) Args(exe string, hide []string) []string {
	args := []string{
		"--dev-bind", "/", "/",
		"--unshare-user", "--disable-userns",
		"--unshare-pid", "--proc", "/proc",
		"--die-with-parent",
	}
	for _, d := range hide {
		args = append(args, "--tmpfs", d)
	}
	return append(args, "--", exe)
}

// NewSandbox is the sandbox this build runs plugins in. It fails closed: a
// build without BwrapPath refuses to run plugins unless SandboxOffEnv=off.
func NewSandbox(grants Grants) (*Sandbox, error) {
	if os.Getenv(SandboxOffEnv) == "off" {
		return nil, nil
	}
	if BwrapPath == "" {
		return nil, fmt.Errorf("this ebac was built without bubblewrap (plugin.BwrapPath): refusing to run plugins unsandboxed; set %s=off to allow it", SandboxOffEnv)
	}
	if len(grants[GrantFigaro]) == 0 {
		return nil, fmt.Errorf("sandbox: no figaro socket to hide")
	}
	return &Sandbox{Bwrap: BwrapPath, Grants: grants}, nil
}
