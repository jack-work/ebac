package pluginrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gluck/ebac/sdk/plugin"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// For names the plugin that serves a source. One plugin may serve several
// sources (ebac-gh: pull requests and author watches).
func For(source string) (string, error) {
	switch source {
	case "", "github-pr", "github-author":
		return "gh", nil
	case "keel-threads", model.SourceKeelBranches:
		return "keel", nil
	case "teams-chat":
		return "tomb", nil
	case model.SourceBandHub:
		return "bandhub", nil
	case model.SourceRoster:
		return "roster", nil
	case model.SourceReleases, model.SourceScout, model.SourceGHCache:
		return "gh", nil
	case model.SourceApprove, model.SourceOrgApprove:
		return "approve", nil
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

func (r *Runner) Call(ctx context.Context, name string, req plugin.Request) (*plugin.Reply, error) {
	req.Protocol = plugin.Version
	in, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: encode request: %w", name, err)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := r.command(ctx, name, &req.Critic)
	cmd.Stdin = bytes.NewReader(in)
	out := &capped{max: plugin.MaxReply}
	cmd.Stdout = out
	err = cmd.Run()
	switch {
	case out.over:
		return nil, fmt.Errorf("plugin %s %s: reply over %d bytes", name, req.Op, plugin.MaxReply)
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("plugin %s %s: timed out after %s", name, req.Op, timeout)
	case err != nil:
		return nil, fmt.Errorf("plugin %s %s: %w", name, req.Op, err)
	}
	return plugin.Decode(name, req.Op, out.Bytes())
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
