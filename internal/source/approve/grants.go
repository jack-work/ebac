package approve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// GrantsFile is band-grants' published list of LIVE grants (0644): a revoked
// grant is removed from it, a pending or denied request never appears in it.
// band-cred reads the same file at each use.
const GrantsFile = "/var/lib/band-grants/grants.json"

// Grant is one row of GrantsFile, as band-grants writes it.
type Grant struct {
	ID         string `json:"id"`
	Principal  string `json:"principal"`
	Credential string `json:"credential"`
	ExpiresAt  int64  `json:"expires_at"`
	ApprovedBy string `json:"approved_by"`
}

// ReadGrants reads path. A missing or unreadable file is an error: the
// caller refuses, never assumes a grant.
func ReadGrants(path string) ([]Grant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Grants []Grant `json:"grants"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Grants, nil
}

// liveGrant is the reason the grant check fails, or "" when a grant for
// exactly principal and credential is live at now (unix s) and was decided
// by a person, not a standing flake approval (flake:<owner>): the Boss
// approves this scope on /grants/, never the flake.
func liveGrant(gs []Grant, principal, credential string, now int64) string {
	why := fmt.Sprintf("no live grant %s for %s", credential, principal)
	for _, g := range gs {
		if g.Principal != principal || g.Credential != credential {
			continue
		}
		switch {
		case g.ExpiresAt <= now:
			why = fmt.Sprintf("grant %s for %s expired at %d", credential, principal, g.ExpiresAt)
		case strings.HasPrefix(g.ApprovedBy, "flake:") || g.ApprovedBy == "":
			why = fmt.Sprintf("grant %s for %s is standing (%q), not a Boss approval", credential, principal, g.ApprovedBy)
		default:
			return ""
		}
	}
	return why
}

// RequestSocket is band-grants' request socket: the filer's uid
// (SO_PEERCRED) is its principal.
const RequestSocket = "/run/band-grants/request.sock"

// Filer files and reads band-grants requests over RequestSocket (HTTP on a
// unix socket). Socket "" is RequestSocket.
type Filer struct {
	Socket  string
	Timeout time.Duration
}

// RequestState is GET /request/<id>: pending, approved, denied, expired.
type RequestState struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	ExpiresAt *int64 `json:"expires_at"`
}

var requestIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// File asks for credential for ttl, with reason; it returns the request id.
func (f Filer) File(ctx context.Context, credential string, ttl time.Duration, reason string) (string, error) {
	body, _ := json.Marshal(map[string]any{"credential": credential, "ttl_s": int(ttl.Seconds()), "reason": reason})
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := f.do(ctx, http.MethodPost, "/request", body, &out); err != nil {
		return "", err
	}
	if !requestIDRe.MatchString(out.ID) {
		return "", fmt.Errorf("band-grants: request filed, but its id %q is not 12 hex digits", out.ID)
	}
	return out.ID, nil
}

// State reads one of this principal's requests.
func (f Filer) State(ctx context.Context, id string) (RequestState, error) {
	var st RequestState
	if !requestIDRe.MatchString(id) {
		return st, fmt.Errorf("band-grants: request id %q is not 12 hex digits", id)
	}
	err := f.do(ctx, http.MethodGet, "/request/"+id, nil, &st)
	return st, err
}

func (f Filer) do(ctx context.Context, method, path string, body []byte, out any) error {
	sock := f.Socket
	if sock == "" {
		sock = RequestSocket
	}
	to := f.Timeout
	if to == 0 {
		to = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(ctx, method, "http://band-grants"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("band-grants %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("band-grants %s %s: %d %s", method, path, resp.StatusCode, oneLine(e.Error))
	}
	return json.Unmarshal(b, out)
}
