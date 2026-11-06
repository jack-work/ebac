package keel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket is ebac's own door to keel: socket-activated, admits uid 988
// only, forwards GET /api/r/<viewer-id>/threads for listed repos and nothing
// else. Never TCP: ebac's egress rejects every IP connection but its proxy.
const DefaultSocket = "/run/kweb-ebac/threads.sock"

// ErrGone is keel saying the repo is not there for us: the socket is missing
// or refuses connections, or the door answers 404 (unlisted repo).
var ErrGone = errors.New("keel threads gone")

// Socket is EBAC_KEEL_SOCKET, else DefaultSocket.
func Socket() string {
	if s := os.Getenv("EBAC_KEEL_SOCKET"); s != "" {
		return s
	}
	return DefaultSocket
}

// Thread and Comment are the fields ebac reads from keel/viewer/threads.go.
// keel keeps no edit time: an edit is a change of body digest.
type Thread struct {
	ID     string `json:"id"`
	Anchor *struct {
		File      string `json:"file"`
		StartLine int    `json:"startLine"`
	} `json:"anchor"`
	Quote    string    `json:"quote,omitempty"`
	Resolved bool      `json:"resolved"`
	Created  time.Time `json:"created"`
	Comments []Comment `json:"comments"`
}

type Comment struct {
	ID      string    `json:"id"`
	Author  string    `json:"author,omitempty"` // keel's nametag: "alias ⟩ account", account, or author
	Account string    `json:"account,omitempty"`
	Alias   string    `json:"alias,omitempty"`
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
}

// Client reads one repo's threads over a unix socket. No identity header is
// sent: the door sets Remote-User itself and drops any client's.
type Client struct {
	Socket  string
	Timeout time.Duration
}

// ViewerID is keel's id for a repo: owner/name with / replaced by -.
func ViewerID(repo string) string { return strings.ReplaceAll(repo, "/", "-") }

// ThreadsPath is the one request the door forwards.
func ThreadsPath(repo string) string { return "/api/r/" + url.PathEscape(ViewerID(repo)) + "/threads" }

func (c *Client) http() *http.Client {
	t := c.Timeout
	if t == 0 {
		t = 30 * time.Second
	}
	sock := c.Socket
	return &http.Client{Timeout: t, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
}

// Threads returns every thread of repo, path by path as keel places them.
// A body without the paths member is an error, never "no threads".
func (c *Client) Threads(ctx context.Context, repo string) (map[string][]Thread, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://keel"+ThreadsPath(repo), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %s: %v", ErrGone, c.Socket, err)
		}
		return nil, fmt.Errorf("keel %s: %w", c.Socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("keel %s: read: %w", repo, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: GET %s: 404 %s", ErrGone, ThreadsPath(repo), snippet(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keel GET %s: %d %s", ThreadsPath(repo), resp.StatusCode, snippet(body))
	}
	var out struct {
		Paths *map[string][]Thread `json:"paths"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("keel GET %s: %w: %s", ThreadsPath(repo), err, snippet(body))
	}
	if out.Paths == nil {
		return nil, fmt.Errorf("keel GET %s: no paths member: %s", ThreadsPath(repo), snippet(body))
	}
	return *out.Paths, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
