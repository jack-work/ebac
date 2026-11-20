package forge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mints(seen *[]string) int {
	n := 0
	for _, s := range *seen {
		if strings.HasPrefix(s, "GET /cred/") {
			n++
		}
	}
	return n
}

// One mint per ROUND (each round is its own process) hammered band-cred every ~10 s. A token minted over the socket is
// cached in the runtime dir until cacheMargin before its expiry, so a second process does not mint again.
func TestCredCacheAcrossProcesses(t *testing.T) {
	sock, seen := fakePrincipalSocket(t)
	rt := os.Getenv("XDG_RUNTIME_DIR") // the helper's isolated temp dir
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // three "processes": a fresh Cred each, no shared memory
		if tok, err := NewCred("band-cred", CredGitHub).Token(ctx); err != nil || tok != fakeTok {
			t.Fatalf("process %d: %q %v", i, tok, err)
		}
	}
	if n := mints(seen); n != 1 {
		t.Fatalf("3 processes made %d mints; want 1 (cached in %s)", n, rt)
	}
	p := filepath.Join(rt, "ebac-cred", CredGitHub+".json")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file %s: %v mode %v; want 0600", p, err, fi)
	}
	if di, _ := os.Stat(filepath.Dir(p)); di.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode %v; want 0700", di.Mode().Perm())
	}
	// A cached token inside cacheMargin of expiry is NOT used: mint again.
	b, _ := json.Marshal(cachedToken{Token: "gho_NEARLYEXPIRED", ExpiresAt: time.Now().Add(2 * time.Minute).Unix()})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := NewCred("band-cred", CredGitHub).Token(ctx); tok != fakeTok {
		t.Fatalf("a near-expiry cached token was used: %q", tok)
	}
	if n := mints(seen); n != 2 {
		t.Fatalf("near-expiry cache should re-mint: mints=%d want 2", n)
	}
}

// Control: with no runtime dir there is no cross-process cache: every process mints.
func TestCredNoRuntimeDirMintsEveryProcess(t *testing.T) {
	sock, seen := fakePrincipalSocket(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	for i := 0; i < 2; i++ {
		if _, err := NewCred("band-cred", CredGitHub).Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := mints(seen); n != 2 {
		t.Fatalf("without a runtime dir: mints=%d want 2", n)
	}
}

func cacheFiles(t *testing.T) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "ebac-cred"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Only the explicit allowlist rests on disk (band-work leader): a credential not on it is never written.
func TestCredCacheOnlyAllowlisted(t *testing.T) {
	sock, _ := fakePrincipalSocket(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	if _, err := NewCred("band-cred", "other-cred").Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := cacheFiles(t); len(f) != 0 {
		t.Fatalf("a non-listed credential was written: %v", f)
	}
	// control: a listed one IS written
	if _, err := NewCred("band-cred", CredTeams).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := cacheFiles(t); len(f) != 1 || f[0] != CredTeams+".json" {
		t.Fatalf("listed credential not cached: %v", f)
	}
}

// band-cred's no_cache wins, even for a listed credential.
func TestCredCacheHonoursNoCache(t *testing.T) {
	sock, _ := fakePrincipalSocket(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	fakeNoCache = true
	t.Cleanup(func() { fakeNoCache = false })
	if _, err := NewCred("band-cred", CredTeams).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := cacheFiles(t); len(f) != 0 {
		t.Fatalf("a no_cache token was written: %v", f)
	}
}

// An expired entry is DELETED, not just ignored.
func TestCredCacheDeletesExpired(t *testing.T) {
	_, _ = fakePrincipalSocket(t) // isolates XDG_RUNTIME_DIR
	c := NewCred("band-cred", CredTeams)
	p := c.cachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(cachedToken{Token: "gho_OLD", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.readCache(time.Now()); ok {
		t.Fatal("an expired entry was used")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expired entry not deleted: %v", err)
	}
}

// Each round logs which token it used (fingerprint + expiry), mint then cache with the SAME fingerprint; never the token.
func TestCredLogsUseByFingerprint(t *testing.T) {
	sock, _ := fakePrincipalSocket(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	t.Setenv("EBAC_RPC_AUDIT", "1")
	r, w, _ := os.Pipe()
	orig := os.Stderr
	os.Stderr = w
	for i := 0; i < 2; i++ {
		_, _ = NewCred("band-cred", CredTeams).Token(context.Background())
	}
	os.Stderr = orig
	w.Close()
	out := make([]byte, 4096)
	n, _ := r.Read(out)
	log := string(out[:n])
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "source=mint") || !strings.Contains(lines[1], "source=cache") {
		t.Fatalf("want a mint line then a cache line:\n%s", log)
	}
	fp := func(l string) string { i := strings.Index(l, "fp="); return l[i : i+15] }
	if fp(lines[0]) != fp(lines[1]) {
		t.Fatalf("fingerprints differ: %q vs %q", lines[0], lines[1])
	}
	if strings.Contains(log, fakeTok) {
		t.Fatal("the token itself reached the log")
	}
}
