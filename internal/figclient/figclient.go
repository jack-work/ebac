// Package figclient says where ebac's figaro daemon is: one endpoint, from
// configuration, behind which every call takes the same code path. A local
// daemon is a unix socket; a remote one is a wss door with a bearer token.
//
//	""                     EBAC_FIGARO if set, else the local angelus socket
//	"/path" | "unix:/path" the daemon's unix socket
//	"wss://host/door"      a remote door; the token comes from the command in
//	                       EBAC_FIGARO_BEARER_CMD (run without a shell, its
//	                       stdout trimmed), never from argv or a log
package figclient

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jack-work/figaro/api/transport"
)

const (
	// EnvEndpoint names the daemon when no --socket is given.
	EnvEndpoint = "EBAC_FIGARO"
	// EnvBearerCmd is the command that prints a wss door's bearer token.
	EnvBearerCmd = "EBAC_FIGARO_BEARER_CMD"
)

// Endpoint resolves spec (see the package comment) to the daemon's endpoint.
func Endpoint(spec string) (transport.Endpoint, error) {
	if spec == "" {
		spec = os.Getenv(EnvEndpoint)
	}
	switch {
	case spec == "":
		return transport.UnixEndpoint(LocalSocket()), nil
	case strings.HasPrefix(spec, "wss://"):
		cmd := strings.Fields(os.Getenv(EnvBearerCmd))
		if len(cmd) == 0 {
			return transport.Endpoint{}, fmt.Errorf("figaro endpoint %s: set %s to a command that prints its bearer token", spec, EnvBearerCmd)
		}
		return transport.WSSEndpoint(spec, &CmdBearer{Cmd: cmd}), nil
	case strings.HasPrefix(spec, "unix:"):
		spec = strings.TrimPrefix(spec, "unix:")
		fallthrough
	case strings.HasPrefix(spec, "/"):
		return transport.UnixEndpoint(spec), nil
	}
	return transport.Endpoint{}, fmt.Errorf("figaro endpoint %q: want a socket path, unix:/path or wss://host/door", spec)
}

// LocalSocket finds the local daemon. XDG_RUNTIME_DIR is the real source; the
// /run/user fallback exists because a systemd user unit does not always
// inherit it.
func LocalSocket() string {
	if s := os.Getenv("FIGARO_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "figaro", "angelus.sock")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "figaro", "angelus.sock")
}

// CmdBearer runs Cmd for a token and keeps it until the door refuses it.
type CmdBearer struct {
	Cmd []string

	mu    sync.Mutex
	token string
}

// Resolve returns the cached token, or runs Cmd for a new one. An error
// names the command and its stderr, never its stdout: that is the token.
func (b *CmdBearer) Resolve() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" {
		return b.token, nil
	}
	var out, errb bytes.Buffer
	c := exec.Command(b.Cmd[0], b.Cmd[1:]...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("bearer command %s: %w: %.200s", b.Cmd[0], err, strings.TrimSpace(errb.String()))
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		return "", errors.New("bearer command " + b.Cmd[0] + " printed no token")
	}
	b.token = tok
	return tok, nil
}

// Invalidate drops token if it is the cached one, so the next Resolve runs
// the command again.
func (b *CmdBearer) Invalidate(token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == token {
		b.token = ""
	}
	return nil
}

// Describe names an endpoint for messages: scheme and address, no token.
func Describe(ep transport.Endpoint) string {
	return ep.Scheme + ":" + ep.Address
}
