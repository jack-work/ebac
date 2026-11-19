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
}

// refuseSudoFlags returns an error naming the first argument that picks a
// path while ebac runs through the sudo door (sudoUID non-empty). Both flag
// spellings (-name, --name) and both value forms (--name v, --name=v) count;
// --body-file - (stdin) is allowed. Arguments after a bare "--" are values,
// not flags.
func refuseSudoFlags(sudoUID string, args []string) error {
	if sudoUID == "" {
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
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
