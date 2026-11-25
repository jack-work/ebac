package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// The form.delta pushes a live figaro daemon sent for `figaro set` on an
// unbound form (0.85, 2026-11-25, probe): a key in a new gh subtree, a leaf
// under a new want.comments.new.<key>.
const (
	deltaTitle = `{"figaro_id":"@0000000000000000000000000000000a","schema":2,"version":3,"patch":{"object":{"Update":{"gh":{"object":{"Set":{"title":"hello"}}}},"New":{"gh":true}}},"at":1794000000001}`
	deltaNew   = `{"figaro_id":"@0000000000000000000000000000000a","schema":2,"version":4,"patch":{"object":{"Update":{"want":{"object":{"Update":{"comments":{"object":{"Update":{"new":{"object":{"Update":{"k1":{"object":{"Set":{"body":"hi"}}}},"New":{"k1":true}}}},"New":{"new":true}}}},"New":{"comments":true}}}},"New":{"want":true}}},"at":1794000000002}`
)

func TestPathsOfLiveDeltas(t *testing.T) {
	for raw, want := range map[string][]string{
		deltaTitle: {"gh.title"},
		deltaNew:   {"want.comments.new.k1.body"},
		`{"object":{"Set":{"gh":{"title":"x"}},"Delete":{"sync":1}}}`:                                        {"gh", "sync"},
		`{"object":{"Update":{"gh":{"object":{"Update":{"title":{"scalar":{"Before":"a","After":"b"}}}}}}}}`: {"gh.title"},
		`{"object":{"Update":{"sync":{"object":{"Set":{"ack":true}}}}}}`:                                     {"sync.ack"},
	} {
		got, err := Paths(json.RawMessage(raw))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Paths(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := Paths(json.RawMessage(`[`)); err == nil {
		t.Error("an unreadable delta read as a patch")
	}
}

type fakeFollow struct {
	mu    sync.Mutex
	on    map[string]func(json.RawMessage)
	done  map[string]chan struct{}
	fail  map[string]bool
	hang  map[string]bool // a dial that never answers until ctx ends
	stops map[string]int
	n     int
}

func (f *fakeFollow) setFail(id string, v bool) { f.mu.Lock(); f.fail[id] = v; f.mu.Unlock() }
func (f *fakeFollow) tries() int                { f.mu.Lock(); defer f.mu.Unlock(); return f.n }
func (f *fakeFollow) stopped(id string) int     { f.mu.Lock(); defer f.mu.Unlock(); return f.stops[id] }

func newFakeFollow() *fakeFollow {
	return &fakeFollow{on: map[string]func(json.RawMessage){}, done: map[string]chan struct{}{}, fail: map[string]bool{}, stops: map[string]int{}}
}

func (f *fakeFollow) follow(ctx context.Context, id string, on func(json.RawMessage)) (func(), <-chan struct{}, error) {
	f.mu.Lock()
	f.n++
	if f.hang[id] {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	defer f.mu.Unlock()
	if f.fail[id] {
		return nil, nil, errors.New("dial refused")
	}
	d := make(chan struct{})
	f.on[id], f.done[id] = on, d
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stops[id]++
		delete(f.on, id)
	}, d, nil
}

func (f *fakeFollow) push(id, raw string) {
	f.mu.Lock()
	on := f.on[id]
	f.mu.Unlock()
	if on != nil {
		on(json.RawMessage(raw))
	}
}

func (f *fakeFollow) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.done[id])
	delete(f.on, id)
}

type woke struct{ critic, key string }

// rig: a watcher on the fake follow; Wake records each report.
func rig() (*Watcher, *fakeFollow, *wokeLog) {
	ff := newFakeFollow()
	got := &wokeLog{}
	w := &Watcher{Follow: ff.follow, Wake: got.add}
	return w, ff, got
}

type wokeLog struct {
	mu  sync.Mutex
	all []woke
}

func (l *wokeLog) add(c, k string) { l.mu.Lock(); l.all = append(l.all, woke{c, k}); l.mu.Unlock() }
func (l *wokeLog) n() int          { l.mu.Lock(); defer l.mu.Unlock(); return len(l.all) }

// waitFor spins (no sleep) until cond holds: follows are dialled off the caller.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func (f *fakeFollow) has(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.on[id] != nil }

func cacheCritic(name string, children map[string]string) *model.Critic {
	c := &model.Critic{Name: name, Source: model.SourceGHCache}
	c.Runtime.Children = children
	return c
}

const (
	formA = "@0000000000000000000000000000000a"
	formB = "@0000000000000000000000000000000b"
)

func TestSyncFollowsOnlyGitHubCacheEntityForms(t *testing.T) {
	w, ff, _ := rig()
	defer w.Close()
	other := &model.Critic{Name: "scout", Source: model.SourceScout}
	other.Runtime.Children = map[string]string{"x": "@00000000000000000000000000000009"}
	stopped := cacheCritic("old", map[string]string{"y": "@00000000000000000000000000000008"})
	stopped.Runtime.Stopped = true
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": formA, "b": formB}), other, stopped})
	w.Wait() // every dial settled
	if w.Following() != 2 || ff.tries() != 2 || !ff.has(formA) || !ff.has(formB) || ff.has("@00000000000000000000000000000009") || ff.has("@00000000000000000000000000000008") {
		t.Fatalf("followed %v", ff.on)
	}
	// a retired child is dropped and its follow stopped
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": formA})})
	if w.Following() != 1 || ff.has(formB) {
		t.Fatalf("after retire: following %d: %v", w.Following(), ff.on)
	}
}

func TestAWriteFieldChangeIsReported(t *testing.T) {
	w, ff, got := rig()
	defer w.Close()
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"example-repo-00000001": formA})})
	waitFor(t, "the follow", func() bool { return w.Following() == 1 })
	ff.push(formA, deltaTitle)
	ff.push(formA, deltaNew)
	if got.n() != 2 || got.all[0] != (woke{"cache", "example-repo-00000001"}) {
		t.Fatalf("reported %+v, want two reports of the entity", got.all)
	}
}

func TestAReadOnlyChangeIsNotReported(t *testing.T) {
	w, ff, got := rig()
	defer w.Close()
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"k": formA})})
	waitFor(t, "the follow", func() bool { return w.Following() == 1 })
	for _, raw := range []string{
		`{"patch":{"object":{"Update":{"gh":{"object":{"Set":{"state":"MERGED"}}}}}}}`,
		`{"patch":{"object":{"Update":{"sync":{"object":{"Set":{"at":"x"}}}}}}}`,
		`{"patch":{"object":{"Update":{"want":{"object":{"Set":{"other":1}}}}}}}`,
		`{"patch":{"object":{"Set":{"track":1}}}}`,
		`not json`,
	} {
		ff.push(formA, raw)
	}
	if got.n() != 0 {
		t.Fatalf("reported read-only changes: %+v", got.all)
	}
}

// A dropped follow (daemon restart) or a refused one is followed again at the
// next listing, and the dropped connection is closed first.
func TestADroppedOrRefusedFollowIsFollowedAgain(t *testing.T) {
	w, ff, got := rig()
	defer w.Close()
	ws := []*model.Critic{cacheCritic("cache", map[string]string{"k": formA, "j": formB})}
	ff.setFail(formB, true)
	w.Sync(context.Background(), ws)
	waitFor(t, "one follow up, one refused", func() bool { return w.Following() == 1 && ff.tries() == 2 })
	ff.drop(formA)
	ff.setFail(formB, false)
	w.Sync(context.Background(), ws)
	waitFor(t, "both followed", func() bool { return w.Following() == 2 })
	if ff.stopped(formA) != 1 {
		t.Fatalf("the dropped follow of A was stopped %d times, want 1", ff.stopped(formA))
	}
	ff.push(formA, deltaTitle)
	ff.push(formB, deltaTitle)
	if got.n() != 2 {
		t.Fatalf("re-followed forms reported %d changes, want 2", got.n())
	}
}

// Review B2: a follow that never answers blocks neither Sync nor the other
// follows, at most Parallel dial at once, and Close cancels the stuck ones.
func TestAStuckFollowBlocksNeitherSyncNorOthers(t *testing.T) {
	ff := newFakeFollow()
	ff.hang = map[string]bool{formA: true}
	w := &Watcher{Follow: ff.follow, Wake: func(string, string) {}, Parallel: 2}
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": formA, "b": formB})})
	waitFor(t, "B up beside the stuck A", func() bool { return w.Following() == 1 && ff.has(formB) })
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": formA, "b": formB})}) // returns: A is still dialling, not dialled twice
	for i := 0; i < 1000; i++ {
		runtime.Gosched() // let any dial this Sync started reach the fake
	}
	if n := ff.tries(); n != 2 {
		t.Fatalf("%d dial(s), want 2 (A not dialled twice while stuck)", n)
	}
	w.Close() // cancels A's dial and returns
	if w.Following() != 0 {
		t.Fatalf("following %d after Close", w.Following())
	}
}

func TestDialsAreBounded(t *testing.T) {
	ff := newFakeFollow()
	ff.hang = map[string]bool{}
	kids := map[string]string{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("@00000000000000000000000000000%03d", i)
		ff.hang[id] = true
		kids[fmt.Sprint(i)] = id
	}
	w := &Watcher{Follow: ff.follow, Wake: func(string, string) {}, Parallel: 2}
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", kids)})
	waitFor(t, "two dials in flight", func() bool { return ff.tries() == 2 })
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if n := ff.tries(); n != 2 {
		t.Fatalf("%d dials in flight, want at most 2", n)
	}
	w.Close()
}

func TestKeysTravelInTheContext(t *testing.T) {
	if Keys(context.Background()) != nil || Keys(With(context.Background(), nil)) != nil {
		t.Fatal("an ordinary round carries wake keys")
	}
	if got := Keys(With(context.Background(), []string{"a", "b"})); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("keys %q", got)
	}
}
