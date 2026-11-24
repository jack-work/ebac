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
	"slices"
	"sort"
	"strings"
	"sync"
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
	case model.SourceTeamsLive:
		return "teamslive", nil
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

// Runner execs plugins under Sandbox when set. NewRunner fills Found from
// the plugin dir's manifests (Discover); a Runner built in code with Found nil
// (tests) execs Dir/ebac-<name>.
type Runner struct {
	Dir     string
	Timeout time.Duration
	Stderr  io.Writer
	Sandbox *Sandbox
	Found   map[string]Found
	Refused map[string]error

	mu    sync.Mutex
	hello map[string]error
}

// DefaultTimeout bounds one plugin exec: a keel feed round long-polls 30 s.
const DefaultTimeout = 120 * time.Second

// NewRunner discovers the plugins of EBAC_PLUGIN_DIR (one Nix store path the
// fleet sets in ebac's wrapper): only a plugin with a valid manifest whose
// pinned sha256 matches its exe runs (Discover). There is no PATH and no
// next-to-the-core fallback. Plugins run in the build's sandbox (NewSandbox),
// each seeing only the grants it needs.
func NewRunner(grants Grants) (*Runner, error) {
	sb, err := NewSandbox(grants)
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("EBAC_PLUGIN_DIR")
	if dir == "" {
		return nil, fmt.Errorf("EBAC_PLUGIN_DIR is not set: no plugin runs (the fleet sets it to the plugin dir's store path)")
	}
	found, refused, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	for name, why := range refused {
		fmt.Fprintf(os.Stderr, "ebac: plugin %s refused: %v\n", name, why)
	}
	return &Runner{Dir: dir, Timeout: DefaultTimeout, Stderr: os.Stderr, Sandbox: sb, Found: found, Refused: refused}, nil
}

// exe is the path a plugin runs from, or why it may not run.
func (r *Runner) exe(plugin string) (string, error) {
	if r.Found == nil {
		return filepath.Join(r.Dir, "ebac-"+plugin), nil
	}
	if f, ok := r.Found[plugin]; ok {
		return f.Path, nil
	}
	if why, ok := r.Refused[plugin]; ok {
		return "", fmt.Errorf("plugin %s refused: %w", plugin, why)
	}
	return "", fmt.Errorf("plugin %s: no manifest in %s", plugin, r.Dir)
}

// handshake runs op hello once per plugin: the plugin must say the name and
// sources its manifest pins, in this core's protocol major.
func (r *Runner) handshake(ctx context.Context, name string) error {
	if r.Found == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hello == nil {
		r.hello = map[string]error{}
	}
	if err, done := r.hello[name]; done {
		return err
	}
	rep, err := r.run(ctx, name, plugin.Request{Op: plugin.OpHello})
	if err == nil {
		want := r.Found[name]
		got, pin := append([]string(nil), rep.Sources...), append([]string(nil), want.Sources...)
		sort.Strings(got)
		sort.Strings(pin)
		if rep.Name != want.Name || !slices.Equal(got, pin) {
			err = fmt.Errorf("plugin %s: hello says %s serving %v, its manifest pins %s serving %v", name, rep.Name, got, want.Name, pin)
		}
	}
	r.hello[name] = err
	return err
}

// command is the ONE place a plugin process is built: path, sandbox,
// arguments and environment.
func (r *Runner) command(ctx context.Context, plugin, exe string, c *model.Critic) *exec.Cmd {
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
	if err := r.handshake(ctx, name); err != nil {
		return nil, err
	}
	return r.run(ctx, name, req)
}

func (r *Runner) run(ctx context.Context, name string, req plugin.Request) (*plugin.Reply, error) {
	exe, err := r.exe(name)
	if err != nil {
		return nil, err
	}
	req.Protocol, req.ProtocolMinor = plugin.Version, plugin.Minor
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
	cmd := r.command(ctx, name, exe, &req.Critic)
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
