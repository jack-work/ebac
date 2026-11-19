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
	"sync"
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

// BrokerKill asks op kill for one form; a lost answer re-checks the form first (the kill may have applied) and asks
// again only while it still stands; a refusal is final.
func TestBrokerKill(t *testing.T) {
	f := NewFigaro("")
	stillThere := true
	old := formGone
	formGone = func(context.Context, *Figaro, string) bool { return !stillThere }
	t.Cleanup(func() { formGone = old })

	got := fakeBroker(t, `{"ok": true}`)
	if err := f.BrokerKill(context.Background(), "@0123abcd"); err != nil || len(*got) != 1 || (*got)[0]["op"] != "kill" || (*got)[0]["form"] != "@0123abcd" {
		t.Fatalf("kill: %v, broker got %v", err, *got)
	}

	got = fakeBroker(t, ``) // the answer is lost (load) but the kill applied
	stillThere = false
	if err := f.BrokerKill(context.Background(), "@0123abcd"); err != nil || len(*got) != 1 {
		t.Fatalf("lost answer, form gone: %v, %d asks (want done after one)", err, len(*got))
	}

	got = fakeBroker(t, ``) // lost answer, the form still stands: asked again, once
	stillThere = true
	if err := f.BrokerKill(context.Background(), "@0123abcd"); err == nil || len(*got) != 2 {
		t.Fatalf("lost answer, form stands: %v, %d asks (want 2 and an error)", err, len(*got))
	}

	got = fakeBroker(t, `{"ok": false, "error": "not an ebac form"}`)
	if err := f.BrokerKill(context.Background(), "@0123abcd"); err == nil || len(*got) != 1 || !strings.Contains(err.Error(), "not an ebac form") {
		t.Fatalf("refusal: %v, %d asks (want 1, final)", err, len(*got))
	}
}

// A renewal past the broker's cap (an old form: ttl counts from its first write) asks nothing and says so once per
// form; one within the cap asks.
func TestRenewalOverTheBrokerCapSkipsAndSaysOnce(t *testing.T) {
	got := fakeBroker(t, `{"ok": true}`)
	warn := captureWarn(t)
	overCap = sync.Map{}
	f := NewFigaro("")
	for i := 0; i < 3; i++ {
		if f.renewTo(context.Background(), "@0123abcd", 892*time.Hour) {
			t.Fatal("a renewal over the cap asked the broker")
		}
	}
	if len(*got) != 0 || strings.Count(warn.String(), "@0123abcd") != 1 || !strings.Contains(warn.String(), "keepalive") {
		t.Fatalf("over the cap: broker got %v, said %q (want nothing asked, said once)", *got, warn)
	}
	if !f.renewTo(context.Background(), "@0123abcd", 200*time.Hour) || len(*got) != 1 || (*got)[0]["value"] != "200h0m0s" {
		t.Fatalf("within the cap: broker got %v", *got)
	}
}

// The broker socket is EBAC_LIFETIME_SOCK when set, else the literal default in its own directory. RED if the
// default path or the env name changes.
func TestLifetimeSockFromEnvElseTheDefault(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got := lifetimeSock(get); got != "/run/user/1000/figaro-lifetime/sock" {
		t.Fatalf("default %q", got)
	}
	env["EBAC_LIFETIME_SOCK"] = "/x/sock"
	if got := lifetimeSock(get); got != "/x/sock" {
		t.Fatalf("env %q", got)
	}
	env["EBAC_LIFETIME_SOCK"] = ""
	if got := lifetimeSock(get); got != "/run/user/1000/figaro-lifetime/sock" {
		t.Fatalf("empty env %q", got)
	}
}
