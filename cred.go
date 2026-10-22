package main

import (
	"bytes"
	"context"
	"fmt"
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
	credGitHub = "ghe-ebac"
	credTeams  = "teams-ebac"
)

func NewCred(bin, name string) *Cred {
	if bin == "" {
		bin = "band-cred"
	}
	return &Cred{Bin: bin, Name: name, Reuse: 5 * time.Minute, Timeout: 60 * time.Second}
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
	cmd := execCommandContext(ctx, c.Bin, "token", c.Name)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "credential_stale") {
			return "", fmt.Errorf("band-cred: %s is stale, the Boss must sign in again (band-cred login %s): %s", c.Name, c.Name, msg)
		}
		return "", fmt.Errorf("band-cred token %s: %s", c.Name, msg)
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" || strings.ContainsAny(tok, " \n\t") {
		return "", fmt.Errorf("band-cred token %s: answer is not one token (%d bytes)", c.Name, len(tok))
	}
	c.tok, c.at = tok, time.Now()
	return tok, nil
}
