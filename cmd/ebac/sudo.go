package main

import (
	"fmt"
	"strings"
)

// The sudo door: an operator runs ebac as uid ebac through sudo (the wrapper
// in pkgs/ebac.nix), which resets the environment (measured 11-19: an
// EBAC_SOCKET set by the caller does not reach ebac) but passes every
// argument. So while SUDO_UID is set, no ARGUMENT may choose what ebac (with
// ebac's credentials and state) executes, dials, reads or authenticates to:
// a binary, a socket, a host, a URL, a config, a credential command, or a
// local path. Each such flag below is refused, whichever command it rides on;
// the values ebac needs come from its own unit and wrapper instead.
var sudoRefused = map[string]string{
	"gh":            "the gh binary ebac executes with its GitHub token",
	"socket":        "a socket ebac dials (figaro, band-hub, band-grants)",
	"host":          "the forge host ebac authenticates to",
	"config":        "ebacd's configuration file",
	"keel-url":      "a keel URL ebac reads with a bearer token",
	"token-cmd":     "a command ebac runs to obtain a bearer token",
	"clone":         "a local clone ebac fetches PR heads through",
	"worktree-root": "a directory ebac checks PR heads out under",
	"notes":         "a path handed to the reviewer seat as guidance",
	"bin":           "the binary the installed units execute",
	"body-file":     "a file ebac reads and posts (use - for stdin)",
	"credential":    "the band-grants credential ebac asks for",
	"principal":     "the band-grants principal a critic files or checks as",
	"grant":         "the band-grants credential a critic requires",
	"approver":      "whose approval a tap is taken from",
}

// refuseSudoFlags returns an error naming the first argument that picks a
// path while ebac runs through the sudo door (sudoUID non-empty). Both flag
// spellings (-name, --name) and both value forms (--name v, --name=v) count;
// --body-file - (stdin) is allowed. EVERY token is scanned, also after a bare
// "--" and in value positions: ebac's own global-flag extraction takes
// --gh/--socket/--host from anywhere in argv, and Go's flag package reads a
// "--" given as another flag's value as that value and keeps parsing (review
// d9d993be F1, F2). A refused name in a value position is refused too: the
// safe side.
func refuseSudoFlags(sudoUID string, args []string) error {
	if sudoUID == "" {
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		why, refused := sudoRefused[name]
		if !refused {
			continue
		}
		if name == "body-file" {
			if !hasVal && i+1 < len(args) {
				val = args[i+1]
			}
			if val == "-" {
				continue
			}
		}
		return fmt.Errorf("--%s refused through the sudo door (SUDO_UID=%s): it would choose %s; ebac takes it from its own unit and wrapper", name, sudoUID, why)
	}
	return nil
}

// sudoPathPrefixes are the only PATH entries kept under the sudo door: sudo
// keeps the caller's PATH (no secure_path), so a bare "systemctl", "gh" or
// "git" would otherwise run a binary the caller chose, as uid ebac (review
// d9d993be F3). The wrapper's own entries are store paths.
var sudoPathPrefixes = []string{"/nix/store/", "/run/current-system/sw/bin", "/run/wrappers/bin", "/etc/profiles/per-user/ebac/bin"}

// sudoPath keeps only the trusted entries of path.
func sudoPath(path string) string {
	var keep []string
	for _, d := range strings.Split(path, ":") {
		for _, p := range sudoPathPrefixes {
			if d == p || strings.HasPrefix(d, p) && (strings.HasSuffix(p, "/") || strings.HasPrefix(d, p+"/")) {
				keep = append(keep, d)
				break
			}
		}
	}
	return strings.Join(keep, ":")
}

// sudoGate is main's sudo-door check: refuse path-picking flags and narrow
// PATH, when SUDO_UID is set. getenv/setenv are os.Getenv/os.Setenv.
func sudoGate(args []string, getenv func(string) string, setenv func(string, string) error) error {
	uid := getenv("SUDO_UID")
	if uid == "" {
		return nil
	}
	if err := refuseSudoFlags(uid, args); err != nil {
		return err
	}
	return setenv("PATH", sudoPath(getenv("PATH")))
}
