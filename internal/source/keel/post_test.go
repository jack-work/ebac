package keel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// threadsDoor answers GET threads with a fixed set.
type threadsDoor struct{ byPath map[string][]Thread }

func (d threadsDoor) Where() string { return "threads" }
func (d threadsDoor) Get(_ context.Context, path string, _ time.Duration) (int, []byte, error) {
	if !strings.HasSuffix(path, "/threads") {
		return 404, []byte("no route"), nil
	}
	b, _ := json.Marshal(map[string]any{"paths": d.byPath})
	return 200, b, nil
}

type call struct {
	method, path, aria, role string
	body                     map[string]any
}

// fakeWriter records every write and answers from status/reply by method.
type fakeWriter struct {
	calls  []call
	status map[string]int    // method -> status (default 200)
	reply  map[string]string // method + " " + path suffix match -> body
	err    error
}

func (f *fakeWriter) Write(_ context.Context, method, path, aria, role string, body []byte) (int, []byte, error) {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	f.calls = append(f.calls, call{method, path, aria, role, m})
	if f.err != nil {
		return 0, nil, f.err
	}
	st := 200
	if s, ok := f.status[method]; ok {
		st = s
	}
	for k, v := range f.reply {
		if k == method+" "+path {
			return st, []byte(v), nil
		}
	}
	return st, []byte(`{"ok":true}`), nil
}

const (
	repo  = "notes/captains-log"
	aria  = "e680885d31ef318df6abd5ffbfe69868"
	role  = "figaro-knight"
	tpath = "/api/r/notes-captains-log/threads"
)

func keelCritic() *model.Critic {
	return &model.Critic{Name: "keel-captains-log", Mode: model.ModeKeel, Write: true, Keel: &model.KeelRef{Repo: repo}}
}

func runPosts(t *testing.T, byPath map[string][]Thread, wr *fakeWriter, posts map[string]*model.Post) {
	t.Helper()
	if byPath == nil {
		byPath = map[string][]Thread{}
	}
	sendPosts(context.Background(), &Client{T: threadsDoor{byPath}}, wr, keelCritic(), posts)
}

func TestKeelReplyPostsToTheThreadWithMarkerAndWriterHeaders(t *testing.T) {
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "answered", ByAria: aria, ByRole: role}
	runPosts(t, map[string][]Thread{"a.md": {{ID: "t1"}}}, wr, map[string]*model.Post{"r1": p})
	if len(wr.calls) != 1 {
		t.Fatalf("calls %+v", wr.calls)
	}
	c := wr.calls[0]
	want := "answered\n\n" + model.PostMarker("keel-captains-log", "r1")
	if c.method != "POST" || c.path != tpath+"/t1/comments" || c.aria != aria || c.role != role || c.body["body"] != want {
		t.Fatalf("call %+v", c)
	}
	if p.Sent == nil || p.Sent.URL != "keel:notes/captains-log#t1" || p.Sent.Recovered || p.Attempts != 1 || p.Error != "" {
		t.Fatalf("post %+v sent %+v", p, p.Sent)
	}
}

func TestKeelNewThreadOnALineReturnsTheNewThreadID(t *testing.T) {
	wr := &fakeWriter{reply: map[string]string{"POST " + tpath: `{"id":"n42","anchor":{}}`}}
	p := &model.Post{PR: repo, Path: "2026/11/19/x.md", Line: 7, Body: "finding", ByAria: aria, ByRole: role}
	runPosts(t, nil, wr, map[string]*model.Post{"f1": p})
	c := wr.calls[0]
	lines, _ := json.Marshal(c.body["lines"])
	if c.method != "POST" || c.path != tpath || c.body["path"] != "2026/11/19/x.md" || string(lines) != "[7,7]" || c.body["rev"] != nil {
		t.Fatalf("call %+v", c)
	}
	if p.Sent == nil || p.Sent.URL != "keel:notes/captains-log#n42" {
		t.Fatalf("sent %+v", p.Sent)
	}
}

func TestKeelNewThreadWithRevAnchorsOnTheDiff(t *testing.T) {
	wr := &fakeWriter{reply: map[string]string{"POST " + tpath: `{"id":"d1"}`}}
	p := &model.Post{PR: repo, Path: "src/a.rs", Line: 930, Rev: "552ad24e", Body: "nit", ByAria: aria, ByRole: role}
	runPosts(t, nil, wr, map[string]*model.Post{"f2": p})
	c := wr.calls[0]
	origin, _ := json.Marshal(c.body["origin"])
	if c.body["rev"] != "552ad24e" || string(origin) != `{"rev":"552ad24e","view":"diff"}` {
		t.Fatalf("call %+v", c)
	}
}

func TestKeelNewThreadWithoutAnIDIsAnErrorNotASend(t *testing.T) {
	wr := &fakeWriter{reply: map[string]string{"POST " + tpath: `{"ok":true}`}}
	p := &model.Post{PR: repo, Path: "a.md", Line: 1, Body: "x", ByAria: aria, ByRole: role}
	runPosts(t, nil, wr, map[string]*model.Post{"f3": p})
	if p.Sent != nil || !strings.Contains(p.Error, "no id") {
		t.Fatalf("post %+v", p)
	}
}

func TestKeelMarkerAlreadyOnTheThreadIsRecoveredNotResent(t *testing.T) {
	marker := model.PostMarker("keel-captains-log", "r1")
	byPath := map[string][]Thread{"a.md": {{ID: "t1", Comments: []Comment{{Body: "jack via Figaro e680885d (figaro-knight): answered\n\n" + marker}}}}}
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "answered", ByAria: aria, ByRole: role, Attempts: 1, Error: "timeout"}
	runPosts(t, byPath, wr, map[string]*model.Post{"r1": p})
	if len(wr.calls) != 0 || p.Sent == nil || !p.Sent.Recovered || p.Error != "" || p.Attempts != 1 {
		t.Fatalf("calls %d post %+v sent %+v", len(wr.calls), p, p.Sent)
	}
}

func TestKeelMarkerOnAnotherThreadDoesNotCountAsSent(t *testing.T) {
	marker := model.PostMarker("keel-captains-log", "r1")
	byPath := map[string][]Thread{"a.md": {{ID: "other", Comments: []Comment{{Body: marker}}}, {ID: "t1"}}}
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "answered", ByAria: aria, ByRole: role}
	runPosts(t, byPath, wr, map[string]*model.Post{"r1": p})
	if len(wr.calls) != 1 || p.Sent == nil || p.Sent.Recovered {
		t.Fatalf("calls %d sent %+v", len(wr.calls), p.Sent)
	}
}

func TestKeelResolveAfterReplyAndSkipWhenAlreadyResolved(t *testing.T) {
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "fixed", Resolve: true, ByAria: aria, ByRole: role}
	runPosts(t, map[string][]Thread{"a.md": {{ID: "t1"}}}, wr, map[string]*model.Post{"r2": p})
	if len(wr.calls) != 2 || wr.calls[1].method != "PATCH" || wr.calls[1].path != tpath+"/t1" ||
		fmt.Sprint(wr.calls[1].body) != "map[resolved:true]" || p.Resolved == "" || p.Attempts != 1 {
		t.Fatalf("calls %+v post %+v", wr.calls, p)
	}
	wr2 := &fakeWriter{}
	p2 := &model.Post{PR: repo, ReplyTo: "t1", Resolve: true, ByAria: aria, ByRole: role}
	runPosts(t, map[string][]Thread{"a.md": {{ID: "t1", Resolved: true}}}, wr2, map[string]*model.Post{"r3": p2})
	// A resolve alone does not read threads first (no body): the PATCH goes out.
	if len(wr2.calls) != 1 || p2.Resolved == "" {
		t.Fatalf("resolve-only: calls %+v post %+v", wr2.calls, p2)
	}
}

func TestKeel4xxIsAFinalRefusalAnd5xxIsARetry(t *testing.T) {
	wr := &fakeWriter{status: map[string]int{"POST": 400}}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria, ByRole: role}
	runPosts(t, nil, wr, map[string]*model.Post{"a": p})
	if !strings.HasPrefix(p.Refused, "keel refused it: 400") || p.Error != "" || !p.Terminal() {
		t.Fatalf("400: %+v", p)
	}
	wr = &fakeWriter{status: map[string]int{"POST": 502}}
	p = &model.Post{PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria, ByRole: role}
	runPosts(t, nil, wr, map[string]*model.Post{"b": p})
	if p.Refused != "" || !strings.Contains(p.Error, "502") || p.Attempts != 1 || p.Terminal() {
		t.Fatalf("502: %+v", p)
	}
}

// cheroot's ask: ebac's own client never sends an unmarked write, whatever
// reaches it.
func TestKeelClientRefusesAnUnmarkedWriteWithoutSending(t *testing.T) {
	for name, p := range map[string]*model.Post{
		"no aria":    {PR: repo, ReplyTo: "t1", Body: "x", ByRole: role},
		"no role":    {PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria},
		"bad role":   {PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria, ByRole: "Knight Role"},
		"short aria": {PR: repo, ReplyTo: "t1", Body: "x", ByAria: "e680", ByRole: role},
	} {
		wr := &fakeWriter{}
		runPosts(t, nil, wr, map[string]*model.Post{"u": p})
		if len(wr.calls) != 0 || !strings.Contains(p.Refused, "unmarked") {
			t.Errorf("%s: calls %d refused %q", name, len(wr.calls), p.Refused)
		}
	}
}

func TestKeelUnreadableThreadsHoldsABodyPostForRetry(t *testing.T) {
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria, ByRole: role}
	sendPosts(context.Background(), &Client{T: cannedDoor{}}, wr, keelCritic(), map[string]*model.Post{"c": p})
	if len(wr.calls) != 0 || !strings.Contains(p.Error, "read threads before posting") || p.Attempts != 0 {
		t.Fatalf("calls %d post %+v", len(wr.calls), p)
	}
}

func TestKeelResolveSkippedWhenTheThreadIsAlreadyResolved(t *testing.T) {
	wr := &fakeWriter{}
	p := &model.Post{PR: repo, ReplyTo: "t1", Body: "fixed", Resolve: true, ByAria: aria, ByRole: role}
	runPosts(t, map[string][]Thread{"a.md": {{ID: "t1", Resolved: true}}}, wr, map[string]*model.Post{"r4": p})
	if len(wr.calls) != 1 || wr.calls[0].method != "POST" || p.Resolved == "" {
		t.Fatalf("calls %+v post %+v", wr.calls, p)
	}
}

func TestKeelPluginHandlesOpPostThroughItsWriter(t *testing.T) {
	wr := &fakeWriter{}
	pl := Plugin{Client: &Client{T: threadsDoor{map[string][]Thread{"a.md": {{ID: "t1"}}}}}, Writer: wr}
	posts := map[string]*model.Post{"r5": {PR: repo, ReplyTo: "t1", Body: "x", ByAria: aria, ByRole: role}}
	rep, err := pl.Handle(context.Background(), plugin.Request{Op: plugin.OpPost, Critic: *keelCritic(), Posts: posts})
	if err != nil || rep.Posts["r5"] == nil || rep.Posts["r5"].Sent == nil || len(wr.calls) != 1 {
		t.Fatalf("rep %+v err %v calls %d", rep, err, len(wr.calls))
	}
}

func TestKeelWriterRoleRuleMatchesTheDoor(t *testing.T) {
	for role, ok := range map[string]bool{"system1-engineer": true, "figaro-knight": true, "Knight": false, "a_b": false, "": false,
		"abcdefghijabcdefghijabcdefghijabcdefghij": true, "abcdefghijabcdefghijabcdefghijabcdefghijk": false} {
		if got := model.KeelWriter(aria, role) == ""; got != ok {
			t.Errorf("role %q accepted=%v, want %v", role, got, ok)
		}
	}
}
