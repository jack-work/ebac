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
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// DefaultSocket is ebac's own door to keel: admits uid 988 named ebac only,
// and only GET /api/r/<viewer-id>/threads and GET /api/v1/r/<viewer-id>/events
// for listed repos.
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

type Transport interface {
	Get(ctx context.Context, path string, timeout time.Duration) (status int, body []byte, err error)
	Where() string
}

type Door struct{ Socket string }

func (d Door) Where() string { return d.Socket }

func (d Door) Get(ctx context.Context, path string, timeout time.Duration) (int, []byte, error) {
	sock := d.Socket
	hc := &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://keel"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return 0, nil, fmt.Errorf("%w: %s: %v", ErrGone, sock, err)
		}
		return 0, nil, fmt.Errorf("keel %s: %w", sock, err)
	}
	return readAll(resp)
}

type HTTPS struct {
	Base     string
	TokenCmd []string
	Client   *http.Client
}

func (h HTTPS) Where() string { return h.Base }

func (h HTTPS) token(ctx context.Context) (string, error) {
	if len(h.TokenCmd) == 0 {
		return "", errors.New("keel: no token command")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.TokenCmd[0], h.TokenCmd[1:]...)
	cmd.Stdin = nil
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keel token command %s failed (%v); its output is not shown", h.TokenCmd[0], exitOnly(err))
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", fmt.Errorf("keel token command %s gave no single token", h.TokenCmd[0])
	}
	return tok, nil
}

func exitOnly(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	return "did not run"
}

func (h HTTPS) Get(ctx context.Context, path string, timeout time.Duration) (int, []byte, error) {
	if !strings.HasPrefix(h.Base, "https://") {
		return 0, nil, fmt.Errorf("keel url %q: want https://", h.Base)
	}
	tok, err := h.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(h.Base, "/")+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("keel %s: bad request", h.Base)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	hc := h.Client
	if hc == nil {
		hc = &http.Client{}
	}
	c := *hc
	c.Timeout = timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("keel %s: %v", h.Base, err)
	}
	return readAll(resp)
}

func readAll(resp *http.Response) (int, []byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("keel: read: %w", err)
	}
	return resp.StatusCode, body, nil
}

// Client reads one repo's threads and events through a Transport. No identity
// header goes through the door: the door reads as its own principal.
type Client struct {
	T       Transport
	Timeout time.Duration
}

func ForRef(ref *model.KeelRef) *Client {
	if ref != nil && ref.URL != "" {
		return &Client{T: HTTPS{Base: ref.URL, TokenCmd: ref.TokenCmd}}
	}
	return &Client{T: Door{Socket: Socket()}}
}

// ViewerID is keel's id for a repo: owner/name with / replaced by -.
func ViewerID(repo string) string { return strings.ReplaceAll(repo, "/", "-") }

func ThreadsPath(repo string) string { return "/api/r/" + url.PathEscape(ViewerID(repo)) + "/threads" }

func EventsPath(repo string, after int64, wait time.Duration) string {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(eventsPage)}}
	if wait > 0 {
		q.Set("wait", wait.String())
	}
	return "/api/v1/r/" + url.PathEscape(ViewerID(repo)) + "/events?" + q.Encode()
}

const eventsPage = 500

func (c *Client) timeout(extra time.Duration) time.Duration {
	t := c.Timeout
	if t == 0 {
		t = 30 * time.Second
	}
	return t + extra
}

func (c *Client) get(ctx context.Context, path string, extra time.Duration) ([]byte, error) {
	status, body, err := c.T.Get(ctx, path, c.timeout(extra))
	if err != nil {
		return nil, err
	}
	shown, _, _ := strings.Cut(path, "?")
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: GET %s: 404 %s", ErrGone, shown, snippet(body))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("keel GET %s: %d %s", shown, status, snippet(body))
	}
	return body, nil
}

// Threads returns every thread of repo, path by path as keel places them.
// A body without the paths member is an error, never "no threads".
func (c *Client) Threads(ctx context.Context, repo string) (map[string][]Thread, error) {
	body, err := c.get(ctx, ThreadsPath(repo), 0)
	if err != nil {
		return nil, err
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

type FeedEvent struct {
	Seq        int64     `json:"seq"`
	Thread     string    `json:"thread"`
	Comment    string    `json:"comment"`
	Kind       string    `json:"kind"`
	Author     string    `json:"author"`
	Signed     string    `json:"signed"`
	Path       string    `json:"path"`
	At         time.Time `json:"at"`
	BodySHA256 string    `json:"bodySha256"`
}

type FeedPage struct {
	Events []FeedEvent `json:"events"`
	Next   *int64      `json:"next"`
}

func (c *Client) Events(ctx context.Context, repo string, after int64, wait time.Duration) ([]FeedEvent, int64, error) {
	path := EventsPath(repo, after, wait)
	body, err := c.get(ctx, path, wait)
	if err != nil {
		return nil, after, err
	}
	var p FeedPage
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, after, fmt.Errorf("keel GET events: %w: %s", err, snippet(body))
	}
	if p.Next == nil || p.Events == nil {
		return nil, after, fmt.Errorf("keel GET events: no events or next member: %s", snippet(body))
	}
	if *p.Next < after {
		return nil, after, fmt.Errorf("keel GET events: next %d is behind after %d", *p.Next, after)
	}
	return p.Events, *p.Next, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
