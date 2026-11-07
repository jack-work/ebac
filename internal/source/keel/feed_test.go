package keel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

const (
	gluck  = "admin ⟩ admin"
	seat   = "Figaro c9ff4e17 ⟩ admin"
	other  = "Figaro 6e9efc9e ⟩ admin"
	notesR = "jack/cs7641-notes"
)

func feedRef() *model.KeelRef {
	return &model.KeelRef{Repo: notesR, Feed: true, WaitingOn: []string{gluck}, Self: []string{seat}}
}

func ev(seq int64, kind, thread, comment, author, signed string) FeedEvent {
	return FeedEvent{Seq: seq, Kind: kind, Thread: thread, Comment: comment, Author: author, Signed: signed, Path: "doc/a.md"}
}

func byPathOf(ts ...Thread) map[string][]Thread { return map[string][]Thread{"doc/a.md": ts} }

func tiers(r keelRound) map[string]model.Tier {
	out := map[string]model.Tier{}
	for _, e := range r.events {
		out[e.Comment+"|"+string(e.Kind)] = e.Tier
	}
	return out
}

func TestExactNametagsAndThePrincipalDecide(t *testing.T) {
	for _, c := range []struct {
		name  string
		e     FeedEvent
		self  bool
		gluck bool
		known bool
	}{
		{"gluck", ev(1, "comment.created", "t", "c", "admin", gluck), false, true, true},
		{"the seat", ev(1, "comment.created", "t", "c", "admin", seat), true, false, true},
		{"another figaro on gluck's account", ev(1, "comment.created", "t", "c", "admin", other), false, false, true},
		{"the seat's nametag as a prefix", ev(1, "comment.created", "t", "c", "admin", "Figaro c9ff4e17x ⟩ admin"), false, false, true},
		{"gluck's nametag as a prefix", ev(1, "comment.created", "t", "c", "admin", "admin ⟩ admin ⟩ x"), false, false, true},
		{"bare account", ev(1, "comment.created", "t", "c", "admin", "admin"), false, false, true},
		{"gluck's text from another principal", ev(1, "comment.created", "t", "c", "eve", gluck), false, false, false},
		{"the seat's text from another principal", ev(1, "comment.created", "t", "c", "eve", seat), false, false, false},
		{"no author", ev(1, "comment.created", "t", "c", "", gluck), false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := feedRef()
			if got := exactly(c.e, r.Self); got != c.self {
				t.Errorf("self %v, want %v", got, c.self)
			}
			if got := exactly(c.e, r.WaitingOn); got != c.gluck {
				t.Errorf("gluck %v, want %v", got, c.gluck)
			}
			if got := knownPrincipal(c.e.Author, r); got != c.known {
				t.Errorf("known %v, want %v", got, c.known)
			}
		})
	}
}

func TestFeedWakesOnGluckAndOnStrangersOnly(t *testing.T) {
	q := th("t1", false,
		cm("g", gluck, "please look"), cm("s", seat, "done"), cm("o", other, "aside"),
		cm("b", "admin", "bare"), cm("e", "Jack ⟩ eve", "from eve"), cm("x", "admin ⟩ admin ⟩ x", "lookalike"))
	evs := []FeedEvent{
		ev(10, "comment.created", "t1", "g", "admin", gluck),
		ev(11, "comment.created", "t1", "s", "admin", seat),
		ev(12, "comment.created", "t1", "o", "admin", other),
		ev(13, "comment.created", "t1", "b", "admin", "admin"),
		ev(14, "comment.created", "t1", "e", "eve", "Jack ⟩ eve"),
		ev(15, "comment.created", "t1", "x", "admin", "admin ⟩ admin ⟩ x"),
		ev(16, "thread.resolved", "t1", "", "admin", gluck),
		ev(17, "thread.reopened", "t1", "", "admin", gluck),
		ev(18, "comment.deleted", "t1", "gone", "admin", "admin"),
		ev(19, "thread.deleted", "t9", "", "admin", gluck),
		ev(20, "comment.edited", "t1", "g", "admin", gluck),
	}
	r := feedDiff(9, feedRef(), evs, byPathOf(q))
	got := tiers(r)
	want := map[string]model.Tier{
		"g|" + string(EvKeelNew):     model.TierWake,
		"o|" + string(EvKeelNew):     model.TierRecord,
		"b|" + string(EvKeelNew):     model.TierRecord,
		"e|" + string(EvKeelNew):     model.TierWake,
		"x|" + string(EvKeelNew):     model.TierRecord,
		"|" + string(EvKeelResolved): model.TierRecord,
		"|" + string(EvKeelReopened): model.TierRecord,
	}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: tier %v, want %v (all %v)", k, got[k], w, got)
		}
	}
	if _, ok := r.items["keel-s"]; ok {
		t.Error("the seat's own comment reached the inbox")
	}
	if it := r.items["keel-g"]; it.By != gluck || it.Body != "please look" {
		t.Errorf("gluck's item %+v", it)
	}
}

func TestADeleteOrResolveNeverWakes(t *testing.T) {
	q := th("t1", false, cm("g", gluck, "hi"))
	for _, kind := range []string{"comment.deleted", "thread.deleted", "thread.resolved", "thread.reopened", "thread.created"} {
		r := feedDiff(0, feedRef(), []FeedEvent{ev(1, kind, "t1", "g", "admin", gluck)}, byPathOf(q))
		for _, e := range r.events {
			if e.Tier == model.TierWake {
				t.Errorf("%s woke the seat", kind)
			}
		}
	}
	r := feedDiff(0, feedRef(), []FeedEvent{ev(1, "comment.deleted", "t1", "g", "admin", "admin")}, byPathOf(q))
	if len(r.events) != 0 {
		t.Errorf("a delete signed admin produced %+v", r.events)
	}
}

func TestFeedSkipsSeenSeqsResolvedThreadsAndOtherPaths(t *testing.T) {
	q := th("t1", true, cm("g", gluck, "hi"))
	r := feedDiff(5, feedRef(), []FeedEvent{ev(5, "comment.created", "t1", "g", "admin", gluck)}, byPathOf(q))
	if len(r.events) != 0 {
		t.Fatalf("seq at the cursor announced: %+v", r.events)
	}
	r = feedDiff(4, feedRef(), []FeedEvent{ev(5, "comment.created", "t1", "g", "admin", gluck)}, byPathOf(q))
	if len(r.events) != 1 || r.events[0].Tier != model.TierRecord {
		t.Fatalf("resolved thread: %+v", r.events)
	}
	ref := feedRef()
	ref.Paths = []string{"other/**"}
	q.Resolved = false
	if r := feedDiff(0, ref, []FeedEvent{ev(1, "comment.created", "t1", "g", "admin", gluck)}, byPathOf(q)); len(r.events) != 0 {
		t.Fatalf("path filter: %+v", r.events)
	}
}

// ---------- rounds through a fake keel ----------

type fakeKeel struct {
	mu      sync.Mutex
	threads map[string][]Thread
	events  []FeedEvent
	status  int
	reqs    []string
	auth    []string
}

func (f *fakeKeel) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.URL.RequestURI())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, "nope")
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/threads"):
			fmt.Fprint(w, payload(f.threads))
		case strings.HasSuffix(r.URL.Path, "/events"):
			var after int64
			fmt.Sscan(r.URL.Query().Get("after"), &after)
			out := []FeedEvent{}
			next := after
			for _, e := range f.events {
				if e.Seq > after {
					out = append(out, e)
					next = e.Seq
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"repo": "x", "events": out, "next": next})
		default:
			w.WriteHeader(404)
		}
	})
}

func unixKeel(t *testing.T, f *fakeKeel) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.handler(t))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return &Client{T: Door{Socket: sock}}
}

func TestFeedRoundsBaselineThenCursor(t *testing.T) {
	old := feedWait
	feedWait = 0
	t.Cleanup(func() { feedWait = old })
	q := th("t1", false, cm("g1", gluck, "old"))
	f := &fakeKeel{threads: byPathOf(q), events: []FeedEvent{ev(1, "comment.created", "t1", "g1", "admin", gluck)}}
	cl := unixKeel(t, f)
	st, ref, ctx := &model.KeelState{}, feedRef(), context.Background()

	r, err := fetchRound(ctx, cl, st, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.events) != 0 || !st.FeedStarted || st.Cursor != 1 || st.Seen != nil || r.index == nil {
		t.Fatalf("baseline: %+v state %+v", r, st)
	}

	r, err = fetchRound(ctx, cl, st, ref)
	if err != nil || r.index != nil || len(r.events) != 0 || st.Cursor != 1 {
		t.Fatalf("quiet round: %+v %v state %+v", r, err, st)
	}

	q.Comments = append(q.Comments, cm("g2", gluck, "new"))
	f.mu.Lock()
	f.threads = byPathOf(q)
	f.events = append(f.events, ev(2, "comment.created", "t1", "g2", "admin", gluck))
	f.mu.Unlock()
	r, err = fetchRound(ctx, cl, st, ref)
	if err != nil || len(r.events) != 1 || r.events[0].Tier != model.TierWake || st.Cursor != 2 {
		t.Fatalf("answer: %+v %v state %+v", r.events, err, st)
	}
	r, _ = fetchRound(ctx, cl, st, ref)
	if len(r.events) != 0 {
		t.Fatalf("the same seq announced twice: %+v", r.events)
	}
	last := f.reqs[len(f.reqs)-1]
	if !strings.Contains(last, "/api/v1/r/jack-cs7641-notes/events?") || !strings.Contains(last, "after=2") || !strings.Contains(last, "limit=500") {
		t.Fatalf("request %s", last)
	}
}

func TestACutOverAnnouncesWhatTheLastThreadsRoundMissed(t *testing.T) {
	old := feedWait
	feedWait = 0
	t.Cleanup(func() { feedWait = old })
	q := th("t1", false, cm("g1", gluck, "old"))
	ref := feedRef()
	ref.Feed = false
	st := &model.KeelState{}
	keelDiff(st, ref, byPathOf(q))
	q.Comments = append(q.Comments, cm("g2", gluck, "between"))
	f := &fakeKeel{threads: byPathOf(q), events: []FeedEvent{ev(7, "comment.created", "t1", "g2", "admin", gluck)}}
	ref.Feed = true
	r, err := fetchRound(context.Background(), unixKeel(t, f), st, ref)
	if err != nil || len(r.events) != 1 || r.events[0].Comment != "g2" || st.Cursor != 7 {
		t.Fatalf("cut-over: %+v %v %+v", r.events, err, st)
	}
}

func TestFeedGoneAndBlind(t *testing.T) {
	old := feedWait
	feedWait = 0
	t.Cleanup(func() { feedWait = old })
	f := &fakeKeel{status: 404}
	cl := unixKeel(t, f)
	st := &model.KeelState{FeedStarted: true, Cursor: 3}
	if _, err := fetchRound(context.Background(), cl, st, feedRef()); !errors.Is(err, ErrGone) {
		t.Fatalf("404: %v", err)
	}
	f.status = 500
	if _, err := fetchRound(context.Background(), cl, st, feedRef()); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("500: %v", err)
	}
	if st.Cursor != 3 {
		t.Fatalf("a failed round moved the cursor to %d", st.Cursor)
	}
	gone := &Client{T: Door{Socket: filepath.Join(t.TempDir(), "none.sock")}}
	if _, err := fetchRound(context.Background(), gone, st, feedRef()); !errors.Is(err, ErrGone) {
		t.Fatalf("missing door: %v", err)
	}
}

func TestEventsRefusesAMalformedPage(t *testing.T) {
	for _, body := range []string{`{"events":[]}`, `{"next":3}`, `not json`, `{"events":[],"next":1}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
		cl := &Client{T: plainHTTP{srv.URL}}
		if _, _, err := cl.Events(context.Background(), notesR, 2, 0); err == nil {
			t.Errorf("%s accepted", body)
		}
		srv.Close()
	}
}

type plainHTTP struct{ base string }

func (p plainHTTP) Where() string { return p.base }
func (p plainHTTP) Get(ctx context.Context, path string, timeout time.Duration) (int, []byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	return readAll(resp)
}

// ---------- HTTPS with a token command ----------

func tokenScript(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}

func tlsKeel(t *testing.T, f *fakeKeel) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func TestHTTPSSendsTheCommandsTokenAndNothingLeaks(t *testing.T) {
	f := &fakeKeel{threads: byPathOf(), events: []FeedEvent{}}
	base, hc := tlsKeel(t, f)
	cl := &Client{T: HTTPS{Base: base, TokenCmd: tokenScript(t, "echo tok-abc123"), Client: hc}}
	if _, _, err := cl.Events(context.Background(), notesR, 0, 0); err != nil {
		t.Fatal(err)
	}
	if f.auth[0] != "Bearer tok-abc123" {
		t.Fatalf("Authorization %q", f.auth[0])
	}

	f.status = 401
	_, _, err := cl.Events(context.Background(), notesR, 0, 0)
	if err == nil || errors.Is(err, ErrGone) || strings.Contains(err.Error(), "tok-abc123") {
		t.Fatalf("401: %v", err)
	}
	f.status = 404
	if _, err := cl.Threads(context.Background(), notesR); !errors.Is(err, ErrGone) {
		t.Fatalf("404: %v", err)
	}

	failing := &Client{T: HTTPS{Base: base, Client: hc, TokenCmd: tokenScript(t, "echo leaked-secret-xyz; echo leaked-secret-xyz >&2; exit 3")}}
	if _, _, err := failing.Events(context.Background(), notesR, 0, 0); err == nil || strings.Contains(err.Error(), "leaked-secret-xyz") || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("failing token command: %v", err)
	}
	for _, out := range []string{"echo", "echo 'two words'", "printf 'a\\nb\\n'"} {
		bad := &Client{T: HTTPS{Base: base, Client: hc, TokenCmd: tokenScript(t, out)}}
		if _, _, err := bad.Events(context.Background(), notesR, 0, 0); err == nil {
			t.Errorf("token output %q accepted", out)
		}
	}
	plain := &Client{T: HTTPS{Base: strings.Replace(base, "https://", "http://", 1), Client: hc, TokenCmd: tokenScript(t, "echo tok")}}
	if _, _, err := plain.Events(context.Background(), notesR, 0, 0); err == nil {
		t.Fatal("http:// accepted")
	}
}

func TestHTTPSFollowsNoRedirect(t *testing.T) {
	var hits int
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"events":[],"next":0}`)
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	hc.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	cl := &Client{T: HTTPS{Base: srv.URL, Client: hc, TokenCmd: tokenScript(t, "echo tok")}}
	if _, _, err := cl.Events(context.Background(), notesR, 0, 0); err == nil || hits != 0 {
		t.Fatalf("redirect followed: hits %d err %v", hits, err)
	}
}

func TestAddKeelTransportForRef(t *testing.T) {
	if _, ok := ForRef(&model.KeelRef{}).T.(Door); !ok {
		t.Fatal("no url: not the door")
	}
	h, ok := ForRef(&model.KeelRef{URL: "https://keel.example", TokenCmd: []string{"hush", "oauth", "get", "ebac"}}).T.(HTTPS)
	if !ok || h.Base != "https://keel.example" || len(h.TokenCmd) != 4 {
		t.Fatalf("url: %+v", h)
	}
}
