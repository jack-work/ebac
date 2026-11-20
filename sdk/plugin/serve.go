package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Caller runs one plugin operation. Runner execs the plugin; InProc runs a
// handler in this process through the same JSON (tests).
type Caller interface {
	Call(ctx context.Context, plugin string, req Request) (*Reply, error)
}

// Handler is a plugin's whole behaviour: one request in, one reply out. An
// error is a failed operation; a round that fails upstream but has effects to
// apply (a blind marker, a failure count) returns a Reply with Error set.
type Handler func(ctx context.Context, req Request) (*Reply, error)

// Decode reads one plugin Reply: unknown fields and any other protocol version
// are refused, so a plugin and a core out of step fail loudly.
func Decode(plugin string, op Op, b []byte) (*Reply, error) {
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
	return Decode(plugin, req.Op, out.Bytes())
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
