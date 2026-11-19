package figclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointSpecs(t *testing.T) {
	t.Setenv("FIGARO_SOCKET", "/run/x/angelus.sock")
	t.Setenv(EnvEndpoint, "")
	t.Setenv(EnvBearerCmd, "printf tok")
	for _, c := range []struct{ spec, scheme, addr string }{
		{"", "unix", "/run/x/angelus.sock"},
		{"/a/b.sock", "unix", "/a/b.sock"},
		{"unix:/a/b.sock", "unix", "/a/b.sock"},
		{"wss://fig.example/door", "wss", "wss://fig.example/door"},
	} {
		ep, err := Endpoint(c.spec)
		if err != nil || ep.Scheme != c.scheme || ep.Address != c.addr {
			t.Errorf("%q: %+v %v, want %s %s", c.spec, ep, err, c.scheme, c.addr)
		}
		if (ep.Bearer != nil) != (c.scheme == "wss") {
			t.Errorf("%q: bearer %v", c.spec, ep.Bearer)
		}
	}
	t.Setenv(EnvEndpoint, "unix:/from/env.sock")
	if ep, _ := Endpoint(""); ep.Address != "/from/env.sock" {
		t.Errorf("EBAC_FIGARO not used: %+v", ep)
	}
	if ep, _ := Endpoint("/flag.sock"); ep.Address != "/flag.sock" {
		t.Errorf("a given spec must win over EBAC_FIGARO: %+v", ep)
	}
	for _, bad := range []string{"relative.sock", "tcp://x", "ws://plain"} {
		if _, err := Endpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	t.Setenv(EnvBearerCmd, "")
	if _, err := Endpoint("wss://fig.example/door"); err == nil || !strings.Contains(err.Error(), EnvBearerCmd) {
		t.Errorf("wss without a bearer command: %v", err)
	}
}

// The token is read from the command's stdout, cached until invalidated, and
// never appears in an error.
func TestCmdBearer(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "n")
	script := filepath.Join(dir, "tok")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+count+"\necho ' s3cret '\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &CmdBearer{Cmd: []string{script}}
	for i := 0; i < 2; i++ {
		if tok, err := b.Resolve(); err != nil || tok != "s3cret" {
			t.Fatalf("resolve: %q %v", tok, err)
		}
	}
	runs := func() int { d, _ := os.ReadFile(count); return strings.Count(string(d), "x") }
	if runs() != 1 {
		t.Fatalf("command ran %d times for two resolves, want 1 (cached)", runs())
	}
	_ = b.Invalidate("other")
	_, _ = b.Resolve()
	if runs() != 1 {
		t.Fatal("invalidating another token dropped the cache")
	}
	_ = b.Invalidate("s3cret")
	_, _ = b.Resolve()
	if runs() != 2 {
		t.Fatalf("invalidated token not refetched: %d runs", runs())
	}

	fail := filepath.Join(dir, "fail")
	_ = os.WriteFile(fail, []byte("#!/bin/sh\necho s3cret\necho denied >&2\nexit 3\n"), 0o755)
	_, err := (&CmdBearer{Cmd: []string{fail}}).Resolve()
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("failing command: %v (want stderr, never stdout)", err)
	}
}
