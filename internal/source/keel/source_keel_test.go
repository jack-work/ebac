package keel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

const boss = "Jack Kelliher ⟩ jack"

func ref() *model.KeelRef {
	return &model.KeelRef{Repo: "jack-work/notes", WaitingOn: []string{"Jack Kelliher"}, Self: []string{"figaro-seat"}}
}

func cm(id, author, body string) Comment {
	alias, account, _ := strings.Cut(author, " ⟩ ")
	if account == "" {
		alias, account = "", author
	}
	return Comment{ID: id, Author: author, Account: account, Alias: alias, Body: body, Created: time.Unix(1, 0)}
}

func th(id string, resolved bool, cs ...Comment) Thread {
	t := Thread{ID: id, Quote: "the selected text", Resolved: resolved, Comments: cs}
	t.Anchor = &struct {
		File      string `json:"file"`
		StartLine int    `json:"startLine"`
	}{File: "doc/a.md", StartLine: 12}
	return t
}

func baseline(t *testing.T, r *model.KeelRef, byPath map[string][]Thread) *model.KeelState {
	t.Helper()
	st := &model.KeelState{}
	ev, items, index := keelDiff(st, r, byPath)
	if len(ev) != 0 || len(items) != 0 {
		t.Fatalf("first round announced %d event(s), %d item(s); it must only record", len(ev), len(items))
	}
	if len(index) == 0 && len(byPath) > 0 {
		t.Fatal("first round wrote no index")
	}
	return st
}

func wakes(ev []model.Event) int {
	n := 0
	for _, e := range ev {
		if e.Tier == model.TierWake {
			n++
		}
	}
	return n
}

func TestKeelFirstRoundSilentThenBossReplyWakes(t *testing.T) {
	q := th("t1", false, cm("c1", "figaro-seat", "question?"))
	st := baseline(t, ref(), map[string][]Thread{"doc/a.md": {q}})
	if st.Seen["c1"] == "" {
		t.Fatalf("baseline did not record c1: %v", st.Seen)
	}
	q.Comments = append(q.Comments, cm("c2", boss, "yes, ship it"))
	ev, items, index := keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}})
	if len(ev) != 1 || ev[0].Kind != EvKeelNew || ev[0].Tier != model.TierWake || ev[0].ThreadID != "t1" || ev[0].Path != "doc/a.md" {
		t.Fatalf("events = %+v", ev)
	}
	it, ok := items["keel-c2"]
	if !ok || it.Body != "yes, ship it" || it.Thread != "t1" || it.Path != "doc/a.md" || it.Line != 12 || it.Quote != "the selected text" ||
		!strings.Contains(it.Fetch, "/api/r/jack-work-notes/threads") {
		t.Fatalf("item = %+v", it)
	}
	if ix := index["t1"]; ix.N != 2 || ix.LastBy != boss || ix.Resolved {
		t.Fatalf("index = %+v", ix)
	}
}

func TestKeelOtherAuthorRecordsOwnIsSilent(t *testing.T) {
	q := th("t1", false, cm("c1", "figaro-seat", "question?"))
	st := baseline(t, ref(), map[string][]Thread{"doc/a.md": {q}})
	q.Comments = append(q.Comments, cm("c2", "Liang Shi ⟩ liang", "my two cents"), cm("c3", "figaro-seat", "follow-up"))
	ev, items, _ := keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}})
	if len(ev) != 1 || ev[0].Comment != "c2" || ev[0].Tier != model.TierRecord {
		t.Fatalf("events = %+v; want only c2, recorded", ev)
	}
	if _, own := items["keel-c3"]; own || items["keel-c2"].Body == "" {
		t.Fatalf("items = %v; want c2 and not the seat's own c3", items)
	}
	if st.Seen["c3"] == "" {
		t.Fatal("own comment not recorded as seen")
	}
}

func TestKeelEditWakesAndUnchangedIsSilent(t *testing.T) {
	q := th("t1", false, cm("c1", boss, "first"))
	st := baseline(t, ref(), map[string][]Thread{"doc/a.md": {q}})
	if ev, _, _ := keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}}); len(ev) != 0 {
		t.Fatalf("an unchanged fetch gave %+v", ev)
	}
	q.Comments[0].Body = "first, amended"
	ev, items, _ := keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}})
	if len(ev) != 1 || ev[0].Kind != EvKeelEdited || ev[0].Tier != model.TierWake || items["keel-c1"].Kind != string(EvKeelEdited) {
		t.Fatalf("edit: events %+v items %v", ev, items)
	}
}

func TestKeelResolvedRecordsAndAnswerInResolvedThreadRecords(t *testing.T) {
	q := th("t1", false, cm("c1", "figaro-seat", "q"))
	st := baseline(t, ref(), map[string][]Thread{"doc/a.md": {q}})
	q.Resolved = true
	q.Comments = append(q.Comments, cm("c2", boss, "done"))
	ev, _, index := keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}})
	if len(ev) != 2 || wakes(ev) != 0 {
		t.Fatalf("resolve + answer in a resolved thread: %+v; want 2 recorded, 0 woken", ev)
	}
	if ev[0].Kind != EvKeelResolved || !index["t1"].Resolved {
		t.Fatalf("resolve not recorded: %+v %+v", ev, index["t1"])
	}
	q.Resolved = false
	ev, _, _ = keelDiff(st, ref(), map[string][]Thread{"doc/a.md": {q}})
	if len(ev) != 1 || ev[0].Kind != EvKeelReopened || ev[0].Tier != model.TierRecord {
		t.Fatalf("reopen: %+v", ev)
	}
}

func TestKeelPathFilter(t *testing.T) {
	r := ref()
	r.Paths = []string{"doc/**", "README.md"}
	a, b := th("ta", false), th("tb", false)
	st := baseline(t, r, map[string][]Thread{"doc/sub/a.md": {a}, "src/main.go": {b}})
	a.Comments = []Comment{cm("ca", boss, "in scope")}
	b.Comments = []Comment{cm("cb", boss, "out of scope")}
	ev, items, index := keelDiff(st, r, map[string][]Thread{"doc/sub/a.md": {a}, "src/main.go": {b}})
	if len(ev) != 1 || ev[0].Comment != "ca" || items["keel-cb"].Body != "" {
		t.Fatalf("path filter: events %+v", ev)
	}
	if _, ok := index["tb"]; ok {
		t.Fatal("an out-of-scope thread reached the index")
	}
	for p, want := range map[string]bool{"README.md": true, "doc/x.md": true, "docs/x.md": false, "x/README.md": false} {
		if got := pathMatches(p, r.Paths); got != want {
			t.Errorf("pathMatches(%q) = %v, want %v", p, got, want)
		}
	}
}

// The waited-on match is whole-name and case-insensitive on any of keel's
// names; a near miss must not wake (negative controls).
func TestKeelWaitedOnAuthorMatch(t *testing.T) {
	r := ref()
	for _, tc := range []struct {
		c    Comment
		want bool
	}{
		{cm("1", boss, ""), true},
		{Comment{Author: "jack"}, false}, // account alone, not listed
		{Comment{Author: "JACK KELLIHER ⟩ jack"}, true},
		{Comment{Alias: "jack kelliher"}, true},
		{Comment{Author: "Jack Kelliherx ⟩ jackx"}, false},
		{Comment{Author: "Jack"}, false},
		{Comment{Author: "Liang Shi ⟩ liang"}, false},
		{Comment{}, false},
	} {
		if got := matches(tc.c, r.WaitingOn); got != tc.want {
			t.Errorf("matches(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
	r.WaitingOn = []string{"jack"}
	if !matches(cm("1", boss, ""), r.WaitingOn) {
		t.Error("account match failed")
	}
	// The same Boss reply from a non-waited author records instead of waking.
	r.WaitingOn = []string{"Someone Else"}
	q := th("t1", false)
	st := baseline(t, r, map[string][]Thread{"doc/a.md": {q}})
	q.Comments = []Comment{cm("c1", boss, "hi")}
	if ev, _, _ := keelDiff(st, r, map[string][]Thread{"doc/a.md": {q}}); len(ev) != 1 || ev[0].Tier != model.TierRecord {
		t.Fatalf("non-waited author: %+v", ev)
	}
}

// ---------- the client against a fake unix-socket door ----------

type door struct {
	mu      sync.Mutex
	status  int
	body    string
	reqs    []*http.Request
	sock    string
	stopped bool
	srv     *httptest.Server
}

func newDoor(t *testing.T) *door {
	t.Helper()
	d := &door{status: 200, body: `{"rev":"abc","paths":{}}`, sock: filepath.Join(t.TempDir(), "threads.sock")}
	ln, err := net.Listen("unix", d.sock)
	if err != nil {
		t.Fatal(err)
	}
	d.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.reqs = append(d.reqs, r)
		w.WriteHeader(d.status)
		_, _ = w.Write([]byte(d.body))
	}))
	d.srv.Listener = ln
	d.srv.Start()
	t.Cleanup(d.stop)
	return d
}

func (d *door) stop() {
	if !d.stopped {
		d.stopped = true
		d.srv.Close()
	}
}

func (d *door) set(status int, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.body = status, body
}

func payload(byPath map[string][]Thread) string {
	b, _ := json.Marshal(map[string]any{"rev": "abc", "paths": byPath, "author": "ebac", "mayWrite": false})
	return string(b)
}

func TestClientGetsThreadsWithoutIdentity(t *testing.T) {
	d := newDoor(t)
	d.set(200, payload(map[string][]Thread{"doc/a.md": {th("t1", false, cm("c1", boss, "hi"))}}))
	got, err := (&Client{Socket: d.sock}).Threads(context.Background(), "jack-work/notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(got["doc/a.md"]) != 1 || got["doc/a.md"][0].Comments[0].Account != "jack" {
		t.Fatalf("decoded %+v", got)
	}
	r := d.reqs[0]
	if r.Method != http.MethodGet || r.URL.Path != "/api/r/jack-work-notes/threads" || r.URL.RawQuery != "" {
		t.Fatalf("request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	for _, h := range []string{"Remote-User", "X-Forwarded-User", "X-Forwarded-Email", "Authorization"} {
		if r.Header.Get(h) != "" {
			t.Errorf("identity header %s sent", h)
		}
	}
}

func TestClientClassifiesGone(t *testing.T) {
	d := newDoor(t)
	cl := &Client{Socket: d.sock}
	ctx := context.Background()
	d.set(404, "404 page not found")
	if _, err := cl.Threads(ctx, "a/b"); !errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
	for _, c := range []struct {
		status int
		body   string
	}{{500, "boom"}, {405, "method"}, {200, `{"rev":"x"}`}, {200, `{"paths":null}`}, {200, "not json"}} {
		d.set(c.status, c.body)
		_, err := cl.Threads(ctx, "a/b")
		if err == nil || errors.Is(err, ErrGone) {
			t.Errorf("%d %q: %v; want a plain failure", c.status, c.body, err)
		}
	}
	d.set(200, `{"paths":{}}`)
	if got, err := cl.Threads(ctx, "a/b"); err != nil || got == nil {
		t.Fatalf("control, empty repo: %v %v", got, err)
	}
	d.stop()
	if _, err := cl.Threads(ctx, "a/b"); !errors.Is(err, ErrGone) {
		t.Fatalf("stopped door: %v", err)
	}
	if _, err := (&Client{Socket: filepath.Join(t.TempDir(), "none.sock")}).Threads(ctx, "a/b"); !errors.Is(err, ErrGone) {
		t.Fatalf("missing socket: %v", err)
	}
}
