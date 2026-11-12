package keelbranch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/keel"
)

const (
	mainSHA  = "1111111111111111111111111111111111111111"
	reviewer = "@189312b5000000000000000000000000"
)

// sha is a commit sha unique in its first 12 hex: the branch's letter, then the commit's number.
func sha(c byte, i int) string { return fmt.Sprintf("%c%011d%028d", c, i, 0) }

type fbranch struct {
	letter  byte
	idx     int
	commits []keel.Commit // newest first, since the merge base
	files   []keel.Change
	time    int64
}

// fakeKeel is keel with branches, threads and call counts.
type fakeKeel struct {
	branches map[string]*fbranch
	threads  map[string][]keel.Thread
	calls    map[string]int
	fail     map[string]error
	revs     []string          // the rev of each ThreadsAt call
	failRev  map[string]error  // ThreadsAt at this rev fails
	log      []keel.FeedEvent  // the event feed: a thread new or changed since the last Events call
	seen     map[string]string // thread id -> digest at the last Events call
}

func newFake() *fakeKeel {
	return &fakeKeel{branches: map[string]*fbranch{}, threads: map[string][]keel.Thread{}, calls: map[string]int{}, fail: map[string]error{},
		seen: map[string]string{}}
}

func (f *fakeKeel) push(name string, n int, files ...string) string {
	b := f.branches[name]
	if b == nil {
		b = &fbranch{}
		f.branches[name] = b
	}
	if b.letter == 0 {
		b.letter, b.idx = byte('a'+len(f.branches)%20), len(f.branches)
	}
	c := keel.Commit{SHA: sha(b.letter, b.idx*100000+len(b.commits)+100*n), Subject: fmt.Sprintf("%s commit %d", name, len(b.commits)+1)}
	b.commits = append([]keel.Commit{c}, b.commits...)
	b.files = nil
	for _, p := range files {
		one := 1
		b.files = append(b.files, keel.Change{Path: p, Status: "M", Additions: &one, Deletions: &one})
	}
	b.time = int64(len(f.branches)*1000 + n)
	return c.SHA
}

func (f *fakeKeel) head(name string) string { return f.branches[name].commits[0].SHA }

func (f *fakeKeel) Refs(_ context.Context, repo string) ([]keel.Ref, error) {
	f.calls["refs"]++
	if err := f.fail["refs"]; err != nil {
		return nil, err
	}
	refs := []keel.Ref{{Name: "main", Kind: "branch", SHA: mainSHA}, {Name: "v1", Kind: "tag", SHA: mainSHA}}
	for name, b := range f.branches {
		refs = append(refs, keel.Ref{Name: name, Kind: "branch", SHA: b.commits[0].SHA, Time: b.time})
	}
	return refs, nil
}

func (f *fakeKeel) find(head string) *fbranch {
	for _, b := range f.branches {
		if b.commits[0].SHA == head {
			return b
		}
	}
	return nil
}

func (f *fakeKeel) Span(_ context.Context, repo, base, head string) (*keel.Span, error) {
	f.calls["span"]++
	if err := f.fail["span"]; err != nil {
		return nil, err
	}
	b := f.find(head)
	n := len(b.commits)
	return &keel.Span{Base: &keel.Commit{SHA: mainSHA}, Head: &b.commits[0], Count: &n, Files: b.files}, nil
}

func (f *fakeKeel) Log(_ context.Context, repo, rev string, n int) ([]keel.Commit, error) {
	f.calls["log"]++
	b := f.find(rev)
	return append(append([]keel.Commit(nil), b.commits...), keel.Commit{SHA: mainSHA, Subject: "main"}), nil
}

func (f *fakeKeel) ThreadsAt(_ context.Context, repo, rev string) (map[string][]keel.Thread, error) {
	f.calls["threads"]++
	f.revs = append(f.revs, rev)
	if err := f.fail["threads"]; err != nil {
		return nil, err
	}
	if err := f.failRev[rev]; err != nil {
		return nil, err
	}
	return f.threads, nil
}

// Events turns every thread new or changed since the last call into a feed event, then answers after.
func (f *fakeKeel) Events(_ context.Context, repo string, after int64, _ time.Duration) ([]keel.FeedEvent, int64, error) {
	f.calls["events"]++
	if err := f.fail["events"]; err != nil {
		return nil, after, err
	}
	var paths []string
	for p := range f.threads {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, t := range f.threads[p] {
			b, _ := json.Marshal(t)
			if d := string(b); f.seen[t.ID] != d {
				f.seen[t.ID] = d
				f.log = append(f.log, keel.FeedEvent{Seq: int64(len(f.log) + 1), Thread: t.ID, Path: p, Kind: "comment"})
			}
		}
	}
	var out []keel.FeedEvent
	next := after
	for _, e := range f.log {
		if e.Seq > after {
			out = append(out, e)
			next = e.Seq
		}
	}
	return out, next, nil
}

func (f *fakeKeel) total() int {
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

// thread starts a thread at rev on path:line.
func (f *fakeKeel) thread(id, rev, span, path string, line int, cs ...keel.Comment) *keel.Thread {
	t := keel.Thread{ID: id, Anchor: &keel.Anchor{File: path, StartLine: line}, Comments: cs}
	t.Origin.Rev, t.Origin.Span = rev, span
	f.threads[path] = append(f.threads[path], t)
	return &f.threads[path][len(f.threads[path])-1]
}

func comment(id, alias, body string, at time.Time) keel.Comment {
	return keel.Comment{ID: id, Alias: alias, Account: "gluck", Body: body, Created: at}
}

// world runs passes as the core would: it mints a child id for every key written, retires on Retire, and keeps
// owned keys.
type world struct {
	t        *testing.T
	k        *fakeKeel
	st       State
	now      time.Time
	children map[string]string
	owned    map[string]map[string]json.RawMessage
	max      int
	forms    map[string]map[string]json.RawMessage // child key -> its keys as written
	critic   map[string]json.RawMessage
	reads    [2]int // the critic's max_thread_reads and sweep_reads (0: defaults)
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, k: newFake(), now: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), children: map[string]string{},
		owned: map[string]map[string]json.RawMessage{}, forms: map[string]map[string]json.RawMessage{}, critic: map[string]json.RawMessage{}}
	w.k.push("work/John-Kelliher/effort-x", 1, "a.go")
	w.k.push("work/chiqun/router", 1, "b.go", "c.go")
	return w
}

func (w *world) pass() Result {
	w.t.Helper()
	r, err := w.try()
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) try() (Result, error) {
	w.now = w.now.Add(2 * time.Minute)
	for k := range w.k.calls {
		delete(w.k.calls, k)
	}
	r, err := Run(context.Background(), w.k, w.st, Pass{Watch: model.KeelBranches{Repo: "aether/aether", Reviewer: reviewer, MaxThreadReads: w.reads[0], SweepReads: w.reads[1]},
		Children: w.children, Owned: w.owned, Max: w.max, Now: w.now})
	if err != nil {
		return r, err
	}
	w.st = r.State
	for _, fw := range r.Forms {
		if fw.Retire {
			delete(w.children, fw.Key)
			continue
		}
		if w.children[fw.Key] == "" {
			w.children[fw.Key] = fmt.Sprintf("@%032x", len(w.children)+1)
		}
		if w.forms[fw.Key] == nil {
			w.forms[fw.Key] = map[string]json.RawMessage{}
		}
		for k, v := range fw.Set {
			w.forms[fw.Key][k] = v
		}
	}
	for k, v := range r.Set {
		w.critic[k] = v
	}
	for _, k := range r.Unset {
		delete(w.critic, k)
	}
	return r, nil
}

func writes(r Result) map[string]plugin.FormWrite {
	out := map[string]plugin.FormWrite{}
	for _, fw := range r.Forms {
		out[fw.Key] = fw
	}
	return out
}

func keysOf(m map[string]json.RawMessage) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}

const (
	jk = "john-kelliher--effort-x"
	cr = "chiqun--router"
)

// One form per work branch, named by <owner>--<topic>, with every fact; a quiet pass is two keel calls (refs,
// threads) and writes nothing; the registry names each branch's form once it exists.
func TestFormsPerBranchAndQuietPass(t *testing.T) {
	w := newWorld(t)
	r := w.pass()
	ws := writes(r)
	if len(ws) != 2 || ws[jk].Set == nil || ws[cr].Set == nil {
		t.Fatalf("first pass writes %v", ws)
	}
	if got := keysOf(ws[jk].Set); got != "base branch commits diffstat files head merge_base reviewers threads" {
		t.Fatalf("fact keys %s", got)
	}
	f := ws[jk].Set
	if string(f["branch"]) != `"work/John-Kelliher/effort-x"` || string(f["head"]) != `"`+w.k.head("work/John-Kelliher/effort-x")+`"` ||
		string(f["merge_base"]) != `"`+mainSHA+`"` || string(f["base"]) != `"main"` || string(f["diffstat"]) != `{"files":1,"additions":1,"deletions":1}` {
		t.Fatalf("facts %v", f)
	}
	if !strings.Contains(string(f["commits"]), `"count":1`) || strings.Contains(string(f["commits"]), "main") || !strings.Contains(string(ws[cr].Set["files"]), `"M c.go +1 -1"`) {
		t.Fatalf("commits %s files %s", f["commits"], ws[cr].Set["files"])
	}
	// refs, the feed's head, and per branch span, log and its threads at its head.
	if w.k.total() != 8 || w.k.calls["events"] != 1 || len(r.Pings) != 0 || string(r.Set["branches"]) != `{}` {
		t.Fatalf("first pass: %v calls, pings %v", w.k.calls, r.Pings)
	}
	// Pass 2: the forms exist now, the registry names them; nothing else moves.
	r = w.pass()
	var reg map[string]string
	_ = json.Unmarshal(r.Set["branches"], &reg)
	if len(r.Forms) != 0 || reg["work/John-Kelliher/effort-x"] != w.children[jk] || reg["work/chiqun/router"] != w.children[cr] || w.k.total() != 2 {
		t.Fatalf("pass 2: forms %d registry %v calls %v", len(r.Forms), reg, w.k.calls)
	}
	r = w.pass()
	if len(r.Forms)+len(r.Set)+len(r.Unset)+len(r.Pings) != 0 || w.k.total() != 2 || w.k.calls["refs"] != 1 || w.k.calls["events"] != 1 || w.k.calls["threads"] != 0 {
		t.Fatalf("quiet pass: %d forms, set %v, calls %v", len(r.Forms), r.Set, w.k.calls)
	}
}

// A new head reads that branch's span and log only, and rewrites only the facts that moved.
func TestNewHeadRewritesOnlyWhatMoved(t *testing.T) {
	w := newWorld(t)
	w.pass()
	w.pass()
	w.k.push("work/chiqun/router", 2, "b.go", "c.go")
	r := w.pass()
	ws := writes(r)
	if len(ws) != 1 || w.k.calls["span"] != 1 || w.k.calls["log"] != 1 {
		t.Fatalf("writes %v calls %v", ws, w.k.calls)
	}
	if got := keysOf(ws[cr].Set); got != "commits head" {
		t.Fatalf("moved keys %s", got)
	}
}

// Threads: a branch's are those anchored at its commits or started on its span; each says path:line@sha8 and
// whether it is resolved; reviewers' verdict markers give each one's last verdict and the head it read.
func TestThreadsAndReviewers(t *testing.T) {
	w := newWorld(t)
	hj, hc := w.k.head("work/John-Kelliher/effort-x"), w.k.head("work/chiqun/router")
	at := w.now
	w.k.thread("t1", hj, "", "a.go", 7, comment("c2", "rev-a", "<!-- keel-review verdict:approve head:"+hj[:12]+" -->", at.Add(time.Second)),
		comment("c1", "rev-a", "older, listed later\n<!-- keel-review verdict:request_changes head:"+hj[:12]+" -->", at))
	w.k.thread("t2", mainSHA, "main...work/John-Kelliher/effort-x", "a.go", 1, comment("c3", "rev-b", "<!-- keel-review verdict:comment head:"+hj[:8]+" -->", at))
	w.k.thread("t3", hc, "", "b.go", 2, comment("c4", "rev-a", "not this branch", at)).Resolved = true
	w.k.thread("t4", mainSHA, "", "a.go", 3, comment("c5", "x", "on main", at))
	w.k.thread("t5", "", "", "a.go", 4, comment("c6", "x", "no origin at all", at))
	r := w.pass()
	th := string(writes(r)[jk].Set["threads"])
	if !strings.Contains(th, `"open":2,"resolved":0`) || !strings.Contains(th, `"at":"a.go:7@`+hj[:8]+`"`) || strings.Contains(th, "t3") || strings.Contains(th, "t4") || strings.Contains(th, "t5") {
		t.Fatalf("threads %s", th)
	}
	if rv := string(writes(r)[jk].Set["reviewers"]); !strings.Contains(rv, `"rev-a":{"verdict":"APPROVE","head_read":"`+hj[:12]+`"`) ||
		!strings.Contains(rv, `"rev-b":{"verdict":"COMMENT","head_read":"`+hj[:8]+`"`) {
		t.Fatalf("reviewers %s", rv)
	}
	if th := string(writes(r)[cr].Set["threads"]); !strings.Contains(th, `"open":0,"resolved":1`) {
		t.Fatalf("chiqun threads %s", th)
	}
}

// A new comment on a branch's thread wakes that branch form's holder (one line); a comment on another branch's
// thread does not; a new branch's existing comments are a baseline.
func TestNewCommentWakesTheHolder(t *testing.T) {
	w := newWorld(t)
	hj := w.k.head("work/John-Kelliher/effort-x")
	t1 := w.k.thread("t1", hj, "", "a.go", 7, comment("c1", "rev-a", "first", w.now))
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("baseline pinged %v", r.Pings)
	}
	w.pass()
	t1.Comments = append(t1.Comments, comment("c2", "rev-b", "second", w.now.Add(time.Minute)))
	r := w.pass()
	if len(r.Pings) != 1 || r.Pings[0].To != w.children[jk] || !strings.Contains(r.Pings[0].Text, "work/John-Kelliher/effort-x: 1 new comment(s): a.go:7@"+hj[:8]+" by rev-b") {
		t.Fatalf("pings %v", r.Pings)
	}
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("a seen comment woke again: %v", r.Pings)
	}
	w.k.thread("t9", hj, "", "a.go", 1, comment("c9", "rev-a", "new thread", w.now.Add(time.Minute)))
	if r := w.pass(); len(r.Pings) != 1 || !strings.Contains(r.Pings[0].Text, "a.go:1@") {
		t.Fatalf("new thread: %v", r.Pings)
	}
}

// The Reviewer: nothing while code_ready is unset; a ping when it turns true; one per new head after; none on a
// quiet pass; a branch first seen already code_ready is pinged once.
func TestReviewerPings(t *testing.T) {
	w := newWorld(t)
	w.pass()
	w.k.push("work/John-Kelliher/effort-x", 2, "a.go")
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("new head without code_ready pinged %v", r.Pings)
	}
	w.owned[jk] = map[string]json.RawMessage{"code_ready": json.RawMessage(`true`)}
	r := w.pass()
	if len(r.Pings) != 1 || r.Pings[0].To != reviewer || !strings.Contains(r.Pings[0].Text, "work/John-Kelliher/effort-x code_ready at") ||
		!strings.Contains(r.Pings[0].Text, w.children[jk]) {
		t.Fatalf("code_ready: %v", r.Pings)
	}
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("quiet pass pinged %v", r.Pings)
	}
	h := w.k.push("work/John-Kelliher/effort-x", 3, "a.go")
	if r := w.pass(); len(r.Pings) != 1 || !strings.Contains(r.Pings[0].Text, "new head "+h[:8]+" (code_ready)") {
		t.Fatalf("new head: %v", r.Pings)
	}
	w.owned[jk] = map[string]json.RawMessage{"code_ready": json.RawMessage(`false`)}
	w.k.push("work/John-Kelliher/effort-x", 4, "a.go")
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("code_ready off still pinged %v", r.Pings)
	}
	// First seen already code_ready.
	w2 := newWorld(t)
	w2.owned[jk] = map[string]json.RawMessage{"code_ready": json.RawMessage(`"yes"`)}
	if r := w2.pass(); len(r.Pings) != 1 || !strings.Contains(r.Pings[0].Text, "code_ready at") {
		t.Fatalf("first sight: %v", r.Pings)
	}
}

// A branch gone from keel retires its form (archive rename by the core) and leaves the registry.
func TestGoneBranchRetires(t *testing.T) {
	w := newWorld(t)
	w.pass()
	w.pass()
	delete(w.k.branches, "work/chiqun/router")
	r := w.pass()
	var reg map[string]string
	_ = json.Unmarshal(r.Set["branches"], &reg)
	if !writes(r)[cr].Retire || len(reg) != 1 || w.st.Branches["work/chiqun/router"] != nil {
		t.Fatalf("retire: %v registry %v", writes(r), reg)
	}
}

// What is not followed is said on the critic form (overflow) and in the log: a ref not <owner>/<topic>, a name
// collision, past the cap; a followed branch is never displaced by a newer one; overflow clears when it empties.
func TestOverflow(t *testing.T) {
	w := newWorld(t)
	w.max = 2
	w.pass()
	w.k.push("work/loose", 1)
	w.k.push("work/john-kelliher/Effort.X", 1)
	w.k.push("work/zed/newest", 9)
	r := w.pass()
	var of []string
	_ = json.Unmarshal(r.Set["overflow"], &of)
	got := strings.Join(of, "\n")
	if len(of) != 3 || !strings.Contains(got, "work/loose: not work/<owner>/<topic>") || !strings.Contains(got, "work/john-kelliher/Effort.X: its form name collides with work/John-Kelliher/effort-x") ||
		!strings.Contains(got, "work/zed/newest: over the 2-form cap") || !strings.Contains(strings.Join(r.Log, " "), "NOT FOLLOWED (3)") {
		t.Fatalf("overflow %v log %v", of, r.Log)
	}
	if len(writes(r)) != 0 || len(w.st.Branches) != 2 {
		t.Fatalf("an overflow branch got a form: %v", writes(r))
	}
	for _, b := range []string{"work/loose", "work/john-kelliher/Effort.X", "work/zed/newest"} {
		delete(w.k.branches, b)
	}
	if r := w.pass(); strings.Join(r.Unset, " ") != "overflow" {
		t.Fatalf("overflow not cleared: %v", r.Unset)
	}
}

// A followed branch whose form is gone from runtime.children (never minted, or lost) gets every fact again.
func TestLostFormIsRewrittenWhole(t *testing.T) {
	w := newWorld(t)
	w.pass()
	w.pass()
	delete(w.children, jk)
	r := w.pass()
	if got := keysOf(writes(r)[jk].Set); got != "base branch commits diffstat files head merge_base reviewers threads" || w.k.calls["span"] != 1 {
		t.Fatalf("lost form rewrite: %s, calls %v", got, w.k.calls)
	}
	if len(writes(r)) != 1 {
		t.Fatalf("the other form was rewritten: %v", writes(r))
	}
}

// A read that fails fails the round: last_success, the state and the failure count say so; recovery resets them.
func TestFailedReadKeepsLastSuccess(t *testing.T) {
	for _, what := range []string{"refs", "span"} {
		w := newWorld(t)
		now := w.now
		pl := Plugin{Keel: w.k, Now: func() time.Time { return now }}
		c := model.Critic{Name: "aether-work", Source: model.SourceKeelBranches, Branches: &model.KeelBranches{Repo: "aether/aether"}}
		rep, err := pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c})
		if err != nil || rep.Error != "" || !rep.Critic.Runtime.LastSuccess.Equal(now) || len(rep.Forms) != 2 {
			t.Fatalf("%s: good round %v %q", what, err, rep.Error)
		}
		c = *rep.Critic
		state := string(c.Runtime.KeelBranches)
		now = now.Add(2 * time.Minute)
		w.k.fail[what] = errors.New("keel 502")
		if what == "span" {
			w.k.push("work/chiqun/router", 5)
		}
		rep, _ = pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c})
		if !strings.Contains(rep.Error, "502") || !rep.Critic.Runtime.LastSuccess.Equal(c.Runtime.LastSuccess) ||
			string(rep.Critic.Runtime.KeelBranches) != state || rep.Critic.Runtime.ConsecutiveFailures != 1 || len(rep.Forms)+len(rep.Pings) != 0 {
			t.Fatalf("%s failed: %q success %v failures %d", what, rep.Error, rep.Critic.Runtime.LastSuccess, rep.Critic.Runtime.ConsecutiveFailures)
		}
		c = *rep.Critic
		delete(w.k.fail, what)
		now = now.Add(2 * time.Minute)
		rep, _ = pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c})
		if rep.Error != "" || !rep.Critic.Runtime.LastSuccess.Equal(now) || rep.Critic.Runtime.ConsecutiveFailures != 0 {
			t.Fatalf("%s recovered: %q", what, rep.Error)
		}
	}
}

func TestKeyOf(t *testing.T) {
	for ref, want := range map[string]string{
		"work/John-Kelliher/effort-x": jk, "work/a/b/c": "a--b-c", "work/A_B/x.y": "a-b--x-y",
		"work/x": "", "work//t": "", "work/o/": "", "feat/a/b": "", "work/-o/t": "",
	} {
		got, ok := KeyOf("work/", ref)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v (want %q)", ref, got, ok, want)
		}
	}
}

func TestPingQueue(t *testing.T) {
	st := State{}
	var fresh []plugin.Ping
	for i := 0; i < MaxQueue+5; i++ {
		fresh = append(fresh, plugin.Ping{To: reviewer, Text: fmt.Sprint(i)})
	}
	sent := queuePings(&st, fresh, func(string, ...any) {})
	if len(sent) != MaxPingsPerPass || sent[0].Text != "5" || len(st.Queue) != MaxQueue-MaxPingsPerPass {
		t.Fatalf("sent %d (%s) queued %d", len(sent), sent[0].Text, len(st.Queue))
	}
	sent = queuePings(&st, nil, func(string, ...any) {})
	if len(sent) != MaxPingsPerPass || len(st.Queue) != MaxQueue-2*MaxPingsPerPass {
		t.Fatalf("second: %d %d", len(sent), len(st.Queue))
	}
}

// A repo without the base branch is a failed read, never "no branches".
func TestNoBaseFails(t *testing.T) {
	w := newWorld(t)
	w.pass()
	_, err := Run(context.Background(), w.k, w.st, Pass{Watch: model.KeelBranches{Repo: "aether/aether", Base: "trunk"}, Children: w.children})
	if err == nil || !strings.Contains(err.Error(), "no branch trunk") {
		t.Fatalf("missing base: %v", err)
	}
}

// A state lost (unreadable, reset) while the forms live: every branch is new again, and its existing comments are a
// baseline, not a flood of wakes.
func TestLostStateIsABaseline(t *testing.T) {
	w := newWorld(t)
	w.k.thread("t1", w.k.head("work/John-Kelliher/effort-x"), "", "a.go", 7, comment("c1", "rev-a", "old", w.now))
	w.pass()
	w.pass()
	w.st = State{}
	if r := w.pass(); len(r.Pings) != 0 {
		t.Fatalf("a reset state woke: %v", r.Pings)
	}
}

// Threads are read per branch AT ITS HEAD (keel blames once per sha), and only when the feed touches the branch (a
// thread of its, or a path it changes), its head moved, or a sweep (every SweepEvery passes) comes round; a quiet pass
// is refs and the feed. A thread on a file outside the diff is found by the sweep. A feed that fails reads every branch.
func TestThreadsPerBranchAtItsHead(t *testing.T) {
	w := newWorld(t)
	hj, hc := w.k.head("work/John-Kelliher/effort-x"), w.k.head("work/chiqun/router")
	w.k.thread("t0", hc, "", "b.go", 1, comment("c0", "rev-a", "before the first pass", w.now)) // history the feed already holds
	w.pass()
	revs := strings.Join(w.k.revs, " ")
	if len(w.k.revs) != 2 || !strings.Contains(revs, hj) || !strings.Contains(revs, hc) {
		t.Fatalf("revs %v", w.k.revs)
	}
	// The first pass started the feed at its head: the history does not touch anything again.
	w.k.revs = nil
	if w.pass(); len(w.k.revs) != 0 {
		t.Fatalf("old feed events re-read: %v", w.k.revs)
	}
	// A comment on a path the chiqun branch changes: only that branch is read, at its head.
	w.k.revs = nil
	w.k.thread("t1", hc, "", "c.go", 2, comment("c1", "rev-a", "on c.go", w.now))
	r := w.pass()
	if len(w.k.revs) != 1 || w.k.revs[0] != hc || !strings.Contains(string(writes(r)[cr].Set["threads"]), `"open":2`) {
		t.Fatalf("touched by path: revs %v writes %v", w.k.revs, writes(r))
	}
	// A new head: that branch's threads are read at the new head.
	w.k.revs = nil
	h2 := w.k.push("work/John-Kelliher/effort-x", 2, "a.go")
	w.pass()
	if len(w.k.revs) != 1 || w.k.revs[0] != h2 {
		t.Fatalf("moved: %v", w.k.revs)
	}
	// A thread on a file outside every diff: no read until the sweep, which reads every branch.
	w.k.thread("t5", h2, "", "elsewhere.go", 9, comment("c5", "rev-a", "here", w.now.Add(2*time.Minute)))
	reads := 0
	for i := 0; i < SweepEvery; i++ {
		w.k.revs = nil
		r := w.pass()
		reads += len(w.k.revs)
		if len(w.k.revs) == 2 {
			if !strings.Contains(string(writes(r)[jk].Set["threads"]), "elsewhere.go:9@") {
				t.Fatalf("sweep missed the thread: %v", writes(r))
			}
			break
		}
	}
	if reads != 2 {
		t.Fatalf("sweep: %d reads", reads)
	}
	// A reply on that thread (a known id, on no changed path): its branch alone.
	w.k.revs = nil
	w.k.threads["elsewhere.go"][0].Comments = append(w.k.threads["elsewhere.go"][0].Comments, comment("c6", "rev-b", "reply", w.now.Add(3*time.Minute)))
	if r := w.pass(); len(w.k.revs) != 1 || w.k.revs[0] != h2 || len(r.Pings) != 1 {
		t.Fatalf("touched by thread id: revs %v pings %v", w.k.revs, r.Pings)
	}
	// The feed fails: the round still succeeds and its cursor stays, so the reply made meanwhile is read next pass.
	w.k.revs = nil
	w.k.fail["events"] = errors.New("door 403")
	w.k.threads["elsewhere.go"][0].Comments = append(w.k.threads["elsewhere.go"][0].Comments, comment("c7", "rev-b", "again", w.now.Add(4*time.Minute)))
	r = w.pass()
	if !strings.Contains(strings.Join(r.Log, " "), "feed: door 403") {
		t.Fatalf("feed down: log %v", r.Log)
	}
	delete(w.k.fail, "events")
	w.k.revs = nil
	if r := w.pass(); !containsStr(w.k.revs, h2) || len(r.Pings) != 1 {
		t.Fatalf("after the feed came back: revs %v pings %v", w.k.revs, r.Pings)
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// 86 branches at 2 reads a pass (the live aether-work shape): every branch is read within 43 passes, and from then on
// nothing is deferred: the sweep reads only in leftover slots, never re-queues, and a new head is read the next pass.
func TestBacklogDrainsAndSweepNeverQueues(t *testing.T) {
	w := newWorld(t)
	w.reads = [2]int{2, 2}
	for i := 0; i < 84; i++ {
		w.k.push(fmt.Sprintf("work/o%02d/t", i), 1, fmt.Sprintf("f%02d.go", i))
	}
	read := map[string]bool{}
	deferredAt := 0
	for pass := 1; pass <= 120; pass++ {
		w.k.revs = nil
		r := w.pass()
		if len(w.k.revs) > 2 {
			t.Fatalf("pass %d: %d reads", pass, len(w.k.revs))
		}
		for _, rv := range w.k.revs {
			read[rv] = true
		}
		if strings.Contains(strings.Join(r.Log, " "), "deferred") {
			deferredAt = pass
		}
		if pass == 43 && len(read) != 86 {
			t.Fatalf("after 43 passes %d of 86 branches read", len(read))
		}
	}
	if deferredAt > 43 {
		t.Fatalf("still deferring at pass %d", deferredAt)
	}
	// Steady state: the sweep keeps reading (oldest first, two a pass), and every branch comes round.
	swept := map[string]bool{}
	for i := 0; i < 50; i++ {
		w.k.revs = nil
		r := w.pass()
		if strings.Contains(strings.Join(r.Log, " "), "deferred") {
			t.Fatalf("steady state deferred: %v", r.Log)
		}
		for _, rv := range w.k.revs {
			swept[rv] = true
		}
	}
	if len(swept) < 86 {
		t.Fatalf("the sweep came round %d of 86 branches in 50 passes", len(swept))
	}
	// A new head goes before the sweep, read the next pass.
	h := w.k.push("work/o07/t", 2, "f07.go")
	w.k.revs = nil
	if r := w.pass(); !containsStr(w.k.revs, h) || strings.Contains(strings.Join(r.Log, " "), "deferred") {
		t.Fatalf("new head: revs %v log %v", w.k.revs, r.Log)
	}
}

// A threads read that fails (a cold keel past the timeout) fails nothing else: the pass completes, the other branches
// are read, that branch's last known threads stand and it stays due, read on a later pass. At most max_thread_reads
// reads a pass, the longest due first; the rest wait.
func TestThreadReadsNonFatalAndCapped(t *testing.T) {
	w := newWorld(t)
	w.reads = [2]int{2, 2}
	for _, b := range []string{"work/a/one", "work/b/two", "work/c/three"} {
		w.k.push(b, 1, b+".go")
	}
	// Five branches due on the first pass: two read now, the rest on the next passes, oldest due first.
	r := w.pass()
	if len(w.k.revs) != 2 || !strings.Contains(strings.Join(r.Log, " "), "threads of 3 branch(es) deferred") {
		t.Fatalf("first pass: %d reads, log %v", len(w.k.revs), r.Log)
	}
	// The two read move on (due again now): the three waiting since the first pass go before them.
	w.k.push("work/John-Kelliher/effort-x", 3, "a.go")
	w.k.push("work/a/one", 3, "work/a/one.go")
	w.k.revs = nil
	w.pass()
	if strings.Join(w.k.revs, " ") != w.k.head("work/b/two")+" "+w.k.head("work/c/three") {
		t.Fatalf("not oldest due first: %v", w.k.revs)
	}
	read := map[string]bool{w.k.head("work/b/two"): true, w.k.head("work/c/three"): true}
	for i := 0; i < 4 && len(read) < 5; i++ {
		w.k.revs = nil
		w.pass()
		if len(w.k.revs) > 2 {
			t.Fatalf("pass %d: %d reads", i+2, len(w.k.revs))
		}
		for _, rv := range w.k.revs {
			read[rv] = true
		}
	}
	if len(read) != 5 {
		t.Fatalf("not every branch read: %v", read)
	}
	w.k.revs = nil
	if w.pass(); len(w.k.revs) != 0 {
		t.Fatalf("quiet pass read %v", w.k.revs)
	}
	// One branch's read fails: the pass succeeds, its threads stay as last known, it is retried later.
	hj := w.k.push("work/John-Kelliher/effort-x", 2, "a.go")
	w.k.push("work/chiqun/router", 2, "b.go", "c.go")
	w.k.failRev = map[string]error{hj: errors.New("context deadline exceeded")}
	w.k.revs = nil
	r, err := w.try()
	if err != nil || len(w.k.revs) != 2 || writes(r)[jk].Set["threads"] != nil || writes(r)[cr].Set["head"] == nil ||
		w.st.Branches["work/John-Kelliher/effort-x"].Due == 0 || w.st.Branches["work/chiqun/router"].Due != 0 ||
		!strings.Contains(strings.Join(r.Log, " "), "kept the last known, retried later") {
		t.Fatalf("failed read: err %v revs %v writes %v log %v", err, w.k.revs, writes(r), r.Log)
	}
	w.k.failRev = nil
	w.k.revs = nil
	w.pass()
	if len(w.k.revs) != 1 || w.k.revs[0] != hj || w.st.Branches["work/John-Kelliher/effort-x"].Due != 0 {
		t.Fatalf("retry: revs %v due %d", w.k.revs, w.st.Branches["work/John-Kelliher/effort-x"].Due)
	}
}

// The two caps from the critic: max_thread_reads (default 8) for due branches, sweep_reads (default 2) for the sweep
// in the slots they leave. A burst of new heads drains 8 a pass; a quiet pass is refs, the feed and 2 sweep reads.
func TestReadCaps(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 18; i++ {
		w.k.push(fmt.Sprintf("work/o%02d/t", i), 1, fmt.Sprintf("f%02d.go", i))
	}
	r := w.pass() // 20 new branches: 8 read, 12 deferred
	if len(w.k.revs) != 8 || !strings.Contains(strings.Join(r.Log, " "), "threads of 12 branch(es) deferred") {
		t.Fatalf("burst: %d reads, log %v", len(w.k.revs), r.Log)
	}
	for i := 0; i < 2; i++ {
		w.k.revs = nil
		w.pass()
	}
	// All read (8+8+4: the last pass's 4 spare slots take no sweep: nothing is SweepEvery old yet).
	for i := 0; i < SweepEvery; i++ {
		w.k.revs = nil
		w.pass()
		if n := len(w.k.revs); n > SweepReads {
			t.Fatalf("quiet pass %d: %d reads", i, n)
		}
	}
	w.k.revs = nil
	for k := range w.k.calls {
		delete(w.k.calls, k)
	}
	w.pass()
	if len(w.k.revs) != 2 || w.k.total() != 4 { // the defaults: a quiet pass is 4 calls
		t.Fatalf("quiet pass with the sweep due: %d reads, calls %v", len(w.k.revs), w.k.calls)
	}
	// Configured: 3 due reads, 1 sweep read.
	w.reads = [2]int{3, 1}
	for i := 0; i < 5; i++ {
		w.k.push(fmt.Sprintf("work/o%02d/t", i), 2, fmt.Sprintf("f%02d.go", i))
	}
	w.k.revs = nil
	if r := w.pass(); len(w.k.revs) != 3 || !strings.Contains(strings.Join(r.Log, " "), "threads of 2 branch(es) deferred to later passes (at most 3 reads a pass)") {
		t.Fatalf("configured caps: %d reads, log %v", len(w.k.revs), r.Log)
	}
	w.k.revs = nil
	w.pass() // the last 2 due, then 1 slot left for the sweep, capped at 1
	if len(w.k.revs) != 3 {
		t.Fatalf("2 due + 1 sweep: %d reads", len(w.k.revs))
	}
}
