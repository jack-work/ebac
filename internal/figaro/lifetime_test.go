package figaro

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/gluck/ebac/internal/livetest"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// asEbacOrSkip: the lifetime broker serves uid ebac only, so a live lease
// test means something only when run as ebac.
func asEbacOrSkip(t *testing.T) {
	t.Helper()
	u, err := user.Lookup("ebac")
	if err != nil || u.Uid != strconv.Itoa(os.Getuid()) {
		t.Skip("the lifetime broker serves uid ebac only: run as ebac")
	}
}

// fakeBroker serves LifetimeSock for one test, answering each request with
// answer and recording what it got.
func fakeBroker(t *testing.T, answer string) *[]map[string]any {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "lifetime.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := &[]map[string]any{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req map[string]any
			_ = json.Unmarshal(line, &req)
			*got = append(*got, req)
			_, _ = c.Write([]byte(answer + "\n"))
			_ = c.Close()
		}
	}()
	old := LifetimeSock
	LifetimeSock = sock
	t.Cleanup(func() { LifetimeSock = old })
	return got
}

// captureWarn resets the once-per-process warning and captures it.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldLog := lifetimeLog
	lifetimeLog = &buf
	lifetimeWarned.Store(false)
	t.Cleanup(func() { lifetimeLog = oldLog; lifetimeWarned.Store(false) })
	return &buf
}

func TestSetLeaseAsksTheBrokerForSystemTTL(t *testing.T) {
	got := fakeBroker(t, `{"ok": true}`)
	warn := captureWarn(t)
	f := NewFigaro("")
	if err := f.setLease(context.Background(), "@0123abcd", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("broker got %d requests, want 1", len(*got))
	}
	r := (*got)[0]
	if r["form"] != "@0123abcd" || r["key"] != "system.ttl" || r["value"] != "48h0m0s" {
		t.Fatalf("request %v", r)
	}
	if warn.Len() != 0 {
		t.Fatalf("an accepted request logged: %s", warn)
	}
}

func TestBrokerRefusalOrAbsenceLogsOnceAndCarriesOn(t *testing.T) {
	got := fakeBroker(t, `{"ok": false, "error": "not an ebac form"}`)
	warn := captureWarn(t)
	f := NewFigaro("")
	for i := 0; i < 3; i++ {
		if err := f.setLease(context.Background(), "@0123abcd", time.Hour); err != nil {
			t.Fatalf("refusal must not fail the caller: %v", err)
		}
	}
	if len(*got) != 3 {
		t.Fatalf("broker got %d requests, want 3 (each lease is still asked for)", len(*got))
	}
	if n := strings.Count(warn.String(), "lifetime broker"); n != 1 || !strings.Contains(warn.String(), "not an ebac form") {
		t.Fatalf("refusals logged %d times, want once with the reason: %q", n, warn)
	}

	warn = captureWarn(t)
	LifetimeSock = filepath.Join(t.TempDir(), "absent.sock")
	for i := 0; i < 2; i++ {
		if err := f.setLease(context.Background(), "@0123abcd", time.Hour); err != nil {
			t.Fatalf("absent broker must not fail the caller: %v", err)
		}
	}
	if n := strings.Count(warn.String(), "lifetime broker"); n != 1 {
		t.Fatalf("absent broker logged %d times, want once: %q", n, warn)
	}
}

// Live, any uid: a mint asks the broker (here a fake) for the new form's
// lifetime, FormLease for FormNew and the given lease for FormNewLease.
func TestLiveMintAsksTheBrokerForItsLifetime(t *testing.T) {
	f := liveOrSkip(t)
	got := fakeBroker(t, `{"ok": true}`)
	captureWarn(t)
	ctx := context.Background()
	a, err := f.FormNew(ctx, "ebac-livetest-broker-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, a)
	b, err := f.FormNewLease(ctx, "ebac-livetest-broker-b", 48*time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, b)
	want := []map[string]any{{"form": a, "key": "system.ttl", "value": "168h0m0s"}, {"form": b, "key": "system.ttl", "value": "48h0m0s"}}
	if len(*got) != 2 {
		t.Fatalf("broker got %v, want %v", *got, want)
	}
	for i, w := range want {
		for k, v := range w {
			if (*got)[i][k] != v {
				t.Errorf("request %d: %s = %v, want %v", i, k, (*got)[i][k], v)
			}
		}
	}
}
