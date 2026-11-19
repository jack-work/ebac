package keel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// DefaultWriteSocket is keel's write door for ebac: POST a thread or a
// comment, PATCH a thread's resolved state, nothing else. Every write carries
// the writing seat's aria and role, which the door stamps onto the comment.
const DefaultWriteSocket = "/run/kweb-ebac-write/threads.sock"

func WriteSocket() string {
	if s := os.Getenv("EBAC_KEEL_WRITE_SOCKET"); s != "" {
		return s
	}
	return DefaultWriteSocket
}

// Writer sends one write to keel. status is the HTTP status; a transport
// failure is err.
type Writer interface {
	Write(ctx context.Context, method, path string, aria, role string, body []byte) (status int, reply []byte, err error)
}

type WriteDoor struct{ Socket string }

func (d WriteDoor) Write(ctx context.Context, method, path, aria, role string, body []byte) (int, []byte, error) {
	sock := d.Socket
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, method, "http://keel"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Figaro-Aria", aria)
	req.Header.Set("X-Figaro-Role", role)
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EACCES) {
			return 0, nil, fmt.Errorf("keel write door %s: %v", sock, err)
		}
		return 0, nil, fmt.Errorf("keel write door %s: %w", sock, err)
	}
	return readAll(resp)
}

func writePath(repo string) string { return "/api/r/" + url.PathEscape(ViewerID(repo)) }

// keelRef is a receipt URL for a keel thread: keel has no stable web link
// per comment, so the receipt names repo and thread.
func keelRef(repo, thread string) string { return "keel:" + repo + "#" + thread }

// sendPosts takes the authorized posts as far as they go this round. The
// threads are read once: a post whose marker is already on keel (a write that
// landed but was never acknowledged) is recovered, never sent twice.
func sendPosts(ctx context.Context, cl *Client, wr Writer, w *model.Critic, posts map[string]*model.Post) {
	ids := make([]string, 0, len(posts))
	for id := range posts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var byPath map[string][]Thread
	var readErr error
	needRead := false
	for _, id := range ids {
		if p := posts[id]; p.HasBody() && p.Sent == nil {
			needRead = true
		}
	}
	if needRead {
		byPath, readErr = cl.Threads(ctx, w.Keel.Repo)
	}
	for _, id := range ids {
		sendPost(ctx, wr, w, id, posts[id], byPath, readErr)
	}
}

func sendPost(ctx context.Context, wr Writer, w *model.Critic, id string, post *model.Post, byPath map[string][]Thread, readErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tried := false
	defer func() {
		if tried {
			post.Attempts++
		}
	}()
	repo := w.Keel.Repo
	if post.HasBody() && post.Sent == nil {
		if readErr != nil {
			post.Error = "read threads before posting: " + readErr.Error()
			return
		}
		marker := model.PostMarker(w.Name, id)
		if tid, ok := findMarker(byPath, post, marker); ok {
			post.Sent, post.Error = &model.PostReceipt{URL: keelRef(repo, tid), At: now, Recovered: true}, ""
		} else {
			tried = true
			body := post.Body + "\n\n" + marker
			var tid string
			var err error
			if post.ReplyTo != "" {
				tid = post.ReplyTo
				err = write(ctx, wr, "POST", writePath(repo)+"/threads/"+url.PathEscape(tid)+"/comments", post,
					map[string]any{"body": body}, nil)
			} else {
				nt := map[string]any{"body": body, "path": post.Path, "lines": []int{post.Line, post.Line}}
				if post.Rev != "" {
					nt["rev"] = post.Rev
					nt["origin"] = map[string]string{"view": "diff", "rev": post.Rev}
				}
				var got struct {
					ID string `json:"id"`
				}
				err = write(ctx, wr, "POST", writePath(repo)+"/threads", post, nt, &got)
				if err == nil && got.ID == "" {
					err = fmt.Errorf("keel created a thread but returned no id")
				}
				tid = got.ID
			}
			var refused *refusal
			if errors.As(err, &refused) {
				post.Refused = "keel refused it: " + refused.Error()
				return
			}
			if err != nil {
				post.Error = err.Error()
				return
			}
			post.Sent, post.Error = &model.PostReceipt{URL: keelRef(repo, tid), At: now}, ""
		}
	}
	if post.Resolve && post.Resolved == "" {
		if t := findThread(byPath, post.ReplyTo); t != nil && t.Resolved {
			post.Resolved, post.Error = now, ""
			return
		}
		tried = true
		err := write(ctx, wr, "PATCH", writePath(repo)+"/threads/"+url.PathEscape(post.ReplyTo), post,
			map[string]any{"resolved": true}, nil)
		var refused *refusal
		if errors.As(err, &refused) {
			post.Refused = "keel refused the resolve: " + refused.Error()
			return
		}
		if err != nil {
			post.Error = err.Error()
			return
		}
		post.Resolved, post.Error = now, ""
	}
}

// refusal is a 4xx from the door: final, never retried.
type refusal struct {
	status int
	body   string
}

func (r *refusal) Error() string {
	if r.status == 0 {
		return r.body
	}
	return fmt.Sprintf("%d %s", r.status, r.body)
}

func write(ctx context.Context, wr Writer, method, path string, post *model.Post, body any, out any) error {
	if why := model.KeelWriter(post.ByAria, post.ByRole); why != "" {
		return &refusal{0, "not sent, unmarked: " + why} // never an unstamped write, whatever the core passed
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	status, reply, err := wr.Write(ctx, method, path, post.ByAria, post.ByRole, b)
	if err != nil {
		return err
	}
	shown, _, _ := strings.Cut(path, "?")
	if status >= 400 && status < 500 {
		return &refusal{status, snippet(reply)}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("keel %s %s: %d %s", method, shown, status, snippet(reply))
	}
	if out != nil {
		if err := json.Unmarshal(reply, out); err != nil {
			return fmt.Errorf("keel %s %s: reply: %v", method, shown, err)
		}
	}
	return nil
}

// findMarker finds the thread carrying a post's marker: in the replied
// thread, or for a new thread any thread on the post's path.
func findMarker(byPath map[string][]Thread, post *model.Post, marker string) (string, bool) {
	for p, ts := range byPath {
		if post.ReplyTo == "" && p != post.Path {
			continue
		}
		for _, t := range ts {
			if post.ReplyTo != "" && t.ID != post.ReplyTo {
				continue
			}
			for _, c := range t.Comments {
				if strings.Contains(c.Body, marker) {
					return t.ID, true
				}
			}
		}
	}
	return "", false
}

func findThread(byPath map[string][]Thread, id string) *Thread {
	for _, ts := range byPath {
		for i := range ts {
			if ts[i].ID == id {
				return &ts[i]
			}
		}
	}
	return nil
}
