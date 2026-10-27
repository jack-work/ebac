package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

	mu  sync.Mutex
	tok string
	at  time.Time
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
	if c.tok != "" && time.Since(c.at) < c.Reuse {
		return c.tok, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var tok string
	var err error
	if c.Socket != "" {
		tok, err = c.fromSocket(ctx)
	} else {
		tok, err = c.fromCLI(ctx)
	}
	if err != nil {
		return "", err
	}
	c.tok, c.at = tok, time.Now()
	return tok, nil
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
func (c *Cred) fromSocket(ctx context.Context) (string, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://band-cred/cred/"+url.PathEscape(c.Name), nil)
	if err != nil {
		return "", fmt.Errorf("band-cred %s via %s: %v", c.Name, c.Socket, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("band-cred %s via %s: %v", c.Name, c.Socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("band-cred %s via %s: http %d, reading body: %v", c.Name, c.Socket, resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name      string `json:"name"`
			Token     string `json:"token"`
			ExpiresAt int64  `json:"expires_at"`
		}
		if err := json.Unmarshal(body, &ok); err != nil {
			return "", fmt.Errorf("band-cred %s via %s: http 200 but the body is not JSON (%d bytes)", c.Name, c.Socket, len(body))
		}
		if ok.Name != "" && ok.Name != c.Name {
			return "", fmt.Errorf("band-cred %s via %s: answered for %q", c.Name, c.Socket, ok.Name)
		}
		return oneToken(c.Name, ok.Token)
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
		return "", fmt.Errorf("band-cred %s via %s: http %d: %s", c.Name, c.Socket, resp.StatusCode, text)
	}
	if e.Error == "credential_stale" {
		msg := e.Stale
		if msg == "" {
			msg = "credential_stale:" + c.Name
		}
		if e.Reason != "" {
			msg += " (" + e.Reason + ")"
		}
		return "", c.staleErr(msg)
	}
	msg := e.Error
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return "", fmt.Errorf("band-cred %s via %s: http %d %s", c.Name, c.Socket, resp.StatusCode, msg)
}
