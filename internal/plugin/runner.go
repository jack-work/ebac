package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// Caller runs one plugin operation. Runner execs the plugin; InProc runs a
// handler in this process through the same JSON (tests).
type Caller interface {
	Call(ctx context.Context, plugin string, req Request) (*Reply, error)
}

// Handler is a plugin's whole behaviour: one request in, one reply out. An
// error is a failed operation; a round that fails upstream but has effects to
// apply (a blind marker, a failure count) returns a Reply with Error set.
type Handler func(ctx context.Context, req Request) (*Reply, error)

// For names the plugin that serves a source. One plugin may serve several
// sources (ebac-gh: pull requests and author watches).
func For(source string) (string, error) {
	switch source {
	case "", "github-pr", "github-author":
		return "gh", nil
	case "keel-threads":
		return "keel", nil
	case "teams-chat":
		return "tomb", nil
	case model.SourceBandHub:
		return "bandhub", nil
	}
	return "", fmt.Errorf("no plugin serves source %q", source)
}

// Runner execs plugins found in Dir as ebac-<name>, under Sandbox when set.
type Runner struct {
	Dir     string
	Timeout time.Duration
	Stderr  io.Writer
	Sandbox *Sandbox
}

// DefaultTimeout bounds one plugin exec: a keel feed round long-polls 30 s.
const DefaultTimeout = 120 * time.Second

// NewRunner finds plugins next to the running core binary: the core and its
// plugins are one Nix output, so this never consults PATH. EBAC_PLUGIN_DIR
// overrides it for tests. Plugins run in the build's sandbox (NewSandbox),
// each seeing only the grants it needs.
func NewRunner(grants Grants) (*Runner, error) {
	sb, err := NewSandbox(grants)
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("EBAC_PLUGIN_DIR")
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate plugins: %w", err)
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return nil, fmt.Errorf("locate plugins: %w", err)
		}
		dir = filepath.Dir(exe)
	}
	return &Runner{Dir: dir, Timeout: DefaultTimeout, Stderr: os.Stderr, Sandbox: sb}, nil
}

// command is the ONE place a plugin process is built: path, sandbox,
// arguments and environment.
func (r *Runner) command(ctx context.Context, plugin string, c *model.Critic) *exec.Cmd {
	exe := filepath.Join(r.Dir, "ebac-"+plugin)
	var cmd *exec.Cmd
	if r.Sandbox != nil {
		cmd = exec.CommandContext(ctx, r.Sandbox.Bwrap, r.Sandbox.Args(exe, r.Sandbox.Hide(plugin, c))...)
	} else {
		cmd = exec.CommandContext(ctx, exe)
	}
	var env []string
	for _, kv := range os.Environ() {
		// Convention, not enforcement (see package doc): a plugin is not told
		// where figaro is.
		if strings.HasPrefix(kv, "FIGARO_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	cmd.Stderr = r.Stderr
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (r *Runner) Call(ctx context.Context, plugin string, req Request) (*Reply, error) {
	req.Protocol = Version
	in, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: encode request: %w", plugin, err)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := r.command(ctx, plugin, &req.Critic)
	cmd.Stdin = bytes.NewReader(in)
	out := &capped{max: MaxReply}
	cmd.Stdout = out
	err = cmd.Run()
	switch {
	case out.over:
		return nil, fmt.Errorf("plugin %s %s: reply over %d bytes", plugin, req.Op, MaxReply)
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("plugin %s %s: timed out after %s", plugin, req.Op, timeout)
	case err != nil:
		return nil, fmt.Errorf("plugin %s %s: %w", plugin, req.Op, err)
	}
	return decode(plugin, req.Op, out.Bytes())
}

func decode(plugin string, op Op, b []byte) (*Reply, error) {
	var rep Reply
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return nil, fmt.Errorf("plugin %s %s: bad reply: %w", plugin, op, err)
	}
	if rep.Protocol != Version {
		return nil, fmt.Errorf("plugin %s %s: protocol %d, want %d", plugin, op, rep.Protocol, Version)
	}
	return &rep, nil
}

// capped is a buffer that stops accepting past max (and says so). The
// buffer is a named field, not embedded: an embedded bytes.Buffer brings
// ReadFrom, which io.Copy prefers, and that would bypass the cap.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errors.New("reply too large")
	}
	return c.buf.Write(p)
}

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// Serve is a plugin's main: read one Request from stdin, run h, write one
// Reply to stdout. A handler error goes to stderr with exit status 1.
func Serve(h Handler) {
	if err := serve(context.Background(), h, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, h Handler, in io.Reader, out io.Writer) error {
	var req Request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	if req.Protocol != Version {
		return fmt.Errorf("protocol %d, this plugin speaks %d", req.Protocol, Version)
	}
	rep, err := h(ctx, req)
	if err != nil {
		return err
	}
	rep.Protocol = Version
	return json.NewEncoder(out).Encode(rep)
}

// InProc runs handlers in this process, through the same JSON encoding as an
// exec, so a field the wire would drop is dropped here too.
type InProc map[string]Handler

func (p InProc) Call(ctx context.Context, plugin string, req Request) (*Reply, error) {
	h, ok := p[plugin]
	if !ok {
		return nil, fmt.Errorf("plugin %s: not found", plugin)
	}
	req.Protocol = Version
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := serve(ctx, h, bytes.NewReader(in), &out); err != nil {
		return nil, fmt.Errorf("plugin %s %s: %w", plugin, req.Op, err)
	}
	return decode(plugin, req.Op, out.Bytes())
}
