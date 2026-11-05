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
