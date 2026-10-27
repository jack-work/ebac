package forge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakePrincipalSocket serves band-cred's principal-socket protocol on a unix
// socket: GET /cred/<name> only, answers keyed by name. It records every
// request so a test can assert what ebac sent.
func fakePrincipalSocket(t *testing.T) (string, *[]string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ebac-bc") // short: sun_path is 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ebac.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := []string{}
	send := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, "/cred/")
		switch {
		case r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/cred/"):
			send(w, 404, map[string]string{"error": "a principal socket serves /cred/<name> only"})
		case name == CredGitHub || name == CredTeams:
			send(w, 200, map[string]any{"name": name, "token": fakeTok, "expires_at": 1893456000})
		case name == "stale-one":
			send(w, 503, map[string]string{"error": "credential_stale", "name": name, "stale": "credential_stale:" + name, "reason": "sign-in expired"})
		case name == "garbled":
			w.WriteHeader(200)
			w.Write([]byte("token=" + fakeTok + " not json"))
		case name == "wrong-name":
			send(w, 200, map[string]any{"name": CredTeams, "token": fakeTok, "expires_at": 1})
		case name == "teapot":
			w.WriteHeader(500)
			w.Write([]byte("upstream exploded"))
		default:
			send(w, 403, map[string]string{"error": "not_granted", "name": name, "reason": "credential '" + name + "' is not granted to principal 'ebac'"})
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, &seen
}

// cliMustNotRun fails the test if any child process is started: the socket
// path must never fall back to the band-cred CLI.
func cliMustNotRun(t *testing.T) {
	t.Helper()
	fakeCredExec(t, "stale") // a CLI mint would answer credential_stale
}

func TestCredSocketMints(t *testing.T) {
	sock, seen := fakePrincipalSocket(t)
	cliMustNotRun(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	c := NewCred("band-cred", CredGitHub)
	if c.Socket != sock {
		t.Fatalf("NewCred did not take EBAC_BAND_CRED_SOCKET: %q", c.Socket)
	}
	tok, err := c.Token(context.Background())
	if err != nil || tok != fakeTok {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if len(*seen) != 1 || (*seen)[0] != "GET /cred/"+CredGitHub {
		t.Fatalf("requests = %q; want exactly GET /cred/%s (no query: a principal socket ignores template)", *seen, CredGitHub)
	}
}

func TestCredSocketRefusals(t *testing.T) {
	sock, _ := fakePrincipalSocket(t)
	cliMustNotRun(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", sock)
	cases := []struct {
		name, want, wantNot string
	}{
		{"denied-one", "http 403 not_granted: credential 'denied-one' is not granted to principal 'ebac'", "the Boss must sign in"},
		{"stale-one", "stale-one is stale, the Boss must sign in again (band-cred login stale-one): credential_stale:stale-one (sign-in expired)", ""},
		{"garbled", "http 200 but the body is not JSON", fakeTok},
		{"wrong-name", `answered for "teams-ebac"`, fakeTok},
		{"teapot", "http 500: upstream exploded", "the Boss must sign in"},
	}
	for _, tc := range cases {
		_, err := NewCred("band-cred", tc.name).Token(context.Background())
		if err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		t.Logf("%s -> %v", tc.name, err)
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, err, tc.want)
		}
		if tc.wantNot != "" && strings.Contains(err.Error(), tc.wantNot) {
			t.Errorf("%s: error %q must not contain %q", tc.name, err, tc.wantNot)
		}
	}
}

func TestCredSocketMissingNamesTheSocket(t *testing.T) {
	cliMustNotRun(t)
	t.Setenv("EBAC_BAND_CRED_SOCKET", "/nonexistent/ebac-test/ebac.sock")
	_, err := NewCred("band-cred", CredGitHub).Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/ebac-test/ebac.sock") {
		t.Fatalf("an unreachable socket must be named in the error: %v", err)
	}
}

// Control: with EBAC_BAND_CRED_SOCKET unset, today's CLI path runs unchanged.
func TestCredSocketUnsetUsesTheCLI(t *testing.T) {
	sock, seen := fakePrincipalSocket(t)
	_ = sock
	t.Setenv("EBAC_BAND_CRED_SOCKET", "")
	fakeCredExec(t, "ok")
	c := NewCred("band-cred", CredGitHub)
	if c.Socket != "" {
		t.Fatalf("Socket = %q with the env unset", c.Socket)
	}
	if tok, err := c.Token(context.Background()); err != nil || tok != fakeTok {
		t.Fatalf("CLI path: %q, %v", tok, err)
	}
	fakeCredExec(t, "stale")
	_, err := NewCred("band-cred", CredGitHub).Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "band-cred login "+CredGitHub) {
		t.Fatalf("CLI path stale: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("the socket was used with the env unset: %q", *seen)
	}
}
