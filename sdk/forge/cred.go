package forge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cred is one band-cred credential (access table rows 1-3, signed by cheroot
// 2026-10-22). ebac holds no login of its own: every gh and tomb call gets a
// short-lived access token minted by band-cred, which keeps the refresh token
// and audits each mint. The token lives in this process's memory and in the
// one child it is handed to; it is never written, logged or put in argv.
type Cred struct {
	Bin  string // the band-cred CLI
	Name string // ghe-ebac, teams-ebac
	// Socket, when set, is band-cred's PRINCIPAL socket for ebac (access
	// table rows 5-6): the token is minted by HTTP over it and Bin is unused.
	// The socket is the identity (principal:ebac); band-cred ignores any
	// template there. NewCred fills it from EBAC_BAND_CRED_SOCKET.
	Socket string
	// Reuse bounds how long one token serves this process. band-cred caches
	// until 10 minutes before expiry, so asking again is cheap; ebac runs one
	// round per process, so in practice this is one mint per round.
	Reuse   time.Duration
	Timeout time.Duration

	mu      sync.Mutex
	tok     string
	at      time.Time
	exp     int64 // unix expiry of tok (socket path); 0 = unknown
	noCache bool  // band-cred said this token may not rest anywhere
}

const (
	CredGitHub = "ghe-ebac"
	CredTeams  = "teams-ebac"
)

func NewCred(bin, name string) *Cred {
	if bin == "" {
		bin = "band-cred"
	}
	return &Cred{Bin: bin, Name: name, Socket: os.Getenv("EBAC_BAND_CRED_SOCKET"), Reuse: 5 * time.Minute, Timeout: 60 * time.Second}
}

func (c *Cred) staleErr(msg string) error {
	return fmt.Errorf("band-cred: %s is stale, the Boss must sign in again (band-cred login %s): %s", c.Name, c.Name, msg)
}

// Token returns a token for c.Name. A stale credential (band-cred answers
// credential_stale:<name>) needs the Boss to sign in again, and says so.
func (c *Cred) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.fresh(time.Now()) {
		return c.tok, nil
	}
	// Cross-process cache (band-work leader 2026-11-05: one mint per round, i.e. every ~10 s, kept band-cred's audit
	// stream from ever going quiet): a socket-minted token is kept until cacheMargin before its expiry in a 0600 file in
	// the RUNTIME dir (tmpfs, uid ebac only, gone at reboot), never on persistent disk.
	if c.Socket != "" && cacheable[c.Name] {
		if tok, exp, ok := c.readCache(time.Now()); ok {
			c.tok, c.at, c.exp = tok, time.Now(), exp
			c.logUse("cache")
			return tok, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var tok string
	var exp int64
	var err error
	if c.Socket != "" {
		tok, exp, err = c.fromSocket(ctx)
	} else {
		tok, err = c.fromCLI(ctx)
	}
	if err != nil {
		return "", err
	}
	c.tok, c.at, c.exp = tok, time.Now(), exp
	if c.Socket != "" && exp > 0 && cacheable[c.Name] && !c.noCache {
		c.writeCache(tok, exp)
	}
	c.logUse("mint")
	return tok, nil
}

// cacheable is the explicit allowlist of credentials whose access token may rest in the runtime dir (band-work leader,
// 2026-11-05): exactly the ones ebac polls. Any other credential is never written.
var cacheable = map[string]bool{CredGitHub: true, CredTeams: true}

// logUse names, per round, which token a call used (cheroot 2026-11-05: band-cred only audits MINTS, so a cached use is
// visible only here). band-cred has no mint id: the token is identified by a 12-hex SHA-256 fingerprint (never the token)
// and its expiry. Only with EBAC_RPC_AUDIT=1 (ebac's units), so an operator's CLI stays quiet.
func (c *Cred) logUse(source string) {
	if os.Getenv("EBAC_RPC_AUDIT") != "1" || c.tok == "" {
		return
	}
	sum := sha256.Sum256([]byte(c.tok))
	exp := "-"
	if c.exp > 0 {
		exp = time.Unix(c.exp, 0).UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(os.Stderr, "ebac-cred %s source=%s fp=%s expires=%s\n", c.Name, source, hex.EncodeToString(sum[:])[:12], exp)
}

// cacheMargin: a cached token is used until this long before its expiry.
const cacheMargin = 5 * time.Minute

// fresh: a token with a known expiry is good until cacheMargin before it; one without (the CLI path) for Reuse.
func (c *Cred) fresh(now time.Time) bool {
	if c.exp > 0 {
		return time.Unix(c.exp, 0).Sub(now) > cacheMargin
	}
	return now.Sub(c.at) < c.Reuse
}

func (c *Cred) cachePath() string {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" || !filepath.IsAbs(rt) {
		return ""
	}
	return filepath.Join(rt, "ebac-cred", c.Name+".json")
}

type cachedToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

func (c *Cred) readCache(now time.Time) (string, int64, bool) {
	p := c.cachePath()
	if p == "" {
		return "", 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", 0, false
	}
	var ct cachedToken
	if json.Unmarshal(b, &ct) != nil || ct.Token == "" || time.Unix(ct.ExpiresAt, 0).Sub(now) <= cacheMargin {
		_ = os.Remove(p) // expired, near expiry or unreadable: DELETE, do not just ignore (band-work leader)
		return "", 0, false
	}
	return ct.Token, ct.ExpiresAt, true
}

// writeCache is best effort: a failure only costs the next process a mint.
func (c *Cred) writeCache(tok string, exp int64) {
	p := c.cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(cachedToken{Token: tok, ExpiresAt: exp})
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// fromCLI runs `band-cred token <name>` (gluck's owner socket, phase 1).
func (c *Cred) fromCLI(ctx context.Context) (string, error) {
	cmd := ExecCommandContext(ctx, c.Bin, "token", c.Name)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "credential_stale") {
			return "", c.staleErr(msg)
		}
		return "", fmt.Errorf("band-cred token %s: %s", c.Name, msg)
	}
	return oneToken(c.Name, strings.TrimSpace(out.String()))
}

func oneToken(name, tok string) (string, error) {
	if tok == "" || strings.ContainsAny(tok, " \r\n\t") {
		return "", fmt.Errorf("band-cred token %s: answer is not one token (%d bytes)", name, len(tok))
	}
	return tok, nil
}

// fromSocket mints over band-cred's principal socket: GET /cred/<name>.
// 200 carries {"name","token","expires_at"}; anything else {"error",...}.
// A 200 body is never echoed (it holds the token); an error body is.
func (c *Cred) fromSocket(ctx context.Context) (string, int64, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://band-cred/cred/"+url.PathEscape(c.Name), nil)
	if err != nil {
		return "", 0, fmt.Errorf("band-cred %s via %s: %v", c.Name, c.Socket, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("band-cred %s via %s: %v", c.Name, c.Socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("band-cred %s via %s: http %d, reading body: %v", c.Name, c.Socket, resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name      string `json:"name"`
			Token     string `json:"token"`
			ExpiresAt int64  `json:"expires_at"`
			NoCache   bool   `json:"no_cache"` // band-cred may forbid resting this token anywhere
		}
		if err := json.Unmarshal(body, &ok); err != nil {
			return "", 0, fmt.Errorf("band-cred %s via %s: http 200 but the body is not JSON (%d bytes)", c.Name, c.Socket, len(body))
		}
		if ok.Name != "" && ok.Name != c.Name {
			return "", 0, fmt.Errorf("band-cred %s via %s: answered for %q", c.Name, c.Socket, ok.Name)
		}
		tok, err := oneToken(c.Name, ok.Token)
		c.noCache = ok.NoCache
		return tok, ok.ExpiresAt, err
	}
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Stale  string `json:"stale"`
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		return "", 0, fmt.Errorf("band-cred %s via %s: http %d: %s", c.Name, c.Socket, resp.StatusCode, text)
	}
	if e.Error == "credential_stale" {
		msg := e.Stale
		if msg == "" {
			msg = "credential_stale:" + c.Name
		}
		if e.Reason != "" {
			msg += " (" + e.Reason + ")"
		}
		return "", 0, c.staleErr(msg)
	}
	msg := e.Error
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return "", 0, fmt.Errorf("band-cred %s via %s: http %d %s", c.Name, c.Socket, resp.StatusCode, msg)
}
