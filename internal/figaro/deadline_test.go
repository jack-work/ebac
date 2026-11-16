package figaro

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A daemon that accepts and never answers: the 05:00Z bounce shape.
func hungDaemon(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "angelus.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return sock
}

func TestCallDeadline(t *testing.T) {
	f := NewFigaro(hungDaemon(t))
	f.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := f.ListArias(context.Background())
	if err == nil || !Unavailable(err) || time.Since(start) > 5*time.Second {
		t.Fatalf("hung daemon: err %v after %s; want an Unavailable deadline error within the timeout", err, time.Since(start))
	}

	// Control: with no deadline the same call is still waiting, so the
	// deadline above is what returned it, not the fake daemon.
	g := NewFigaro(hungDaemon(t))
	g.Timeout = 0
	done := make(chan error, 1)
	go func() { _, err := g.ListArias(context.Background()); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("control: an unbounded call returned (%v); the fake daemon is not hanging", err)
	case <-time.After(500 * time.Millisecond):
	}
	g.Close()
}

func TestUnavailable(t *testing.T) {
	missing := NewFigaro(filepath.Join(t.TempDir(), "none.sock"))
	_, err := missing.ListArias(context.Background())
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"missing socket", err, true},
		{"deadline", context.DeadlineExceeded, true},
		{"closed under the call", errors.New("connection closed while waiting for response"), true},
		{"daemon answered with an error", errors.New(`jsonrpc error -32000: attach @x: aria @x not found`), false},
		{"refused by authz", errors.New(`jsonrpc error -32020: figaro.kill: none grants kill here`), false},
		{"nil", nil, false},
	} {
		if got := Unavailable(c.err); got != c.want {
			t.Errorf("%s: Unavailable(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
