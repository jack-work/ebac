package wake

import (
	"context"
	"encoding/json"
	"errors"
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
		`{"object":{"Set":{"gh":{"title":"x"}},"Delete":{"sync":1}}}`:                         {"gh", "sync"},
		`{"object":{"Update":{"gh":{"object":{"Update":{"title":{"scalar":{"Before":"a","After":"b"}}}}}}}}`: {"gh.title"},
		`{"object":{"Update":{"sync":{"object":{"Set":{"ack":true}}}}}}`:                       {"sync.ack"},
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
	mu   sync.Mutex
	on   map[string]func(json.RawMessage)
	done map[string]chan struct{}
	fail map[string]bool
	n    int
}

func newFakeFollow() *fakeFollow {
	return &fakeFollow{on: map[string]func(json.RawMessage){}, done: map[string]chan struct{}{}, fail: map[string]bool{}}
}

func (f *fakeFollow) follow(_ context.Context, id string, on func(json.RawMessage)) (func(), <-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[id] {
		return nil, nil, errors.New("dial refused")
	}
	f.n++
	d := make(chan struct{})
	f.on[id], f.done[id] = on, d
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
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

type woke struct {
	critic, key string
	at          time.Time
}

func rig() (*Watcher, *fakeFollow, *[]woke, *time.Time) {
	ff := newFakeFollow()
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	var got []woke
	w := &Watcher{Follow: ff.follow, Now: func() time.Time { return now }, Wake: func(c, k string, at time.Time) { got = append(got, woke{c, k, at}) }}
	return w, ff, &got, &now
}

func cacheCritic(name string, children map[string]string) *model.Critic {
	c := &model.Critic{Name: name, Source: model.SourceGHCache}
	c.Runtime.Children = children
	return c
}

func TestSyncFollowsOnlyGitHubCacheEntityForms(t *testing.T) {
	w, ff, _, _ := rig()
	other := &model.Critic{Name: "scout", Source: model.SourceScout}
	other.Runtime.Children = map[string]string{"x": "@00000000000000000000000000000009"}
	stopped := cacheCritic("old", map[string]string{"y": "@00000000000000000000000000000008"})
	stopped.Runtime.Stopped = true
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": "@0000000000000000000000000000000a", "b": "@0000000000000000000000000000000b"}), other, stopped})
	if w.Following() != 2 || ff.on["@0000000000000000000000000000000a"] == nil || ff.on["@0000000000000000000000000000000b"] == nil {
		t.Fatalf("following %d: %v", w.Following(), ff.on)
	}
	// a retired child is dropped
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"a": "@0000000000000000000000000000000a"})})
	if w.Following() != 1 || ff.on["@0000000000000000000000000000000b"] != nil {
		t.Fatalf("after retire: following %d: %v", w.Following(), ff.on)
	}
}

func TestAWriteFieldChangeWakesWithinTheDelay(t *testing.T) {
	w, ff, got, now := rig()
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"example-repo-00000001": "@0000000000000000000000000000000a"})})
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	if len(*got) != 1 || (*got)[0] != (woke{"cache", "example-repo-00000001", now.Add(time.Second)}) {
		t.Fatalf("woke %+v, want one wake at now+1s", *got)
	}
}

func TestAReadOnlyChangeDoesNotWake(t *testing.T) {
	w, ff, got, _ := rig()
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"k": "@0000000000000000000000000000000a"})})
	for _, raw := range []string{
		`{"patch":{"object":{"Update":{"gh":{"object":{"Set":{"state":"MERGED"}}}}}}}`,
		`{"patch":{"object":{"Update":{"sync":{"object":{"Set":{"at":"x"}}}}}}}`,
		`{"patch":{"object":{"Update":{"want":{"object":{"Set":{"other":1}}}}}}}`,
		`{"patch":{"object":{"Set":{"track":1}}}}`,
		`not json`,
	} {
		ff.push("@0000000000000000000000000000000a", raw)
	}
	if len(*got) != 0 {
		t.Fatalf("woke on read-only changes: %+v", *got)
	}
}

// A burst rides one wake; a change soon after a wake waits out the 5 s gap.
func TestABurstRidesOneWakeAndAWriteIsSpacedByTheGap(t *testing.T) {
	w, ff, got, now := rig()
	w.Sync(context.Background(), []*model.Critic{cacheCritic("cache", map[string]string{"k": "@0000000000000000000000000000000a"})})
	t0 := *now
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	*now = t0.Add(300 * time.Millisecond)
	ff.push("@0000000000000000000000000000000a", deltaNew)
	*now = t0.Add(900 * time.Millisecond)
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	if len(*got) != 1 {
		t.Fatalf("a burst within the delay woke %d times: %+v", len(*got), *got)
	}
	*now = t0.Add(2 * time.Second) // the first wake ran at t0+1s
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	if len(*got) != 2 || !(*got)[1].at.Equal(t0.Add(time.Second+DefaultGap)) {
		t.Fatalf("wakes %+v, want the second at t0+1s+gap", *got)
	}
	*now = t0.Add(20 * time.Second)
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	if len(*got) != 3 || !(*got)[2].at.Equal(t0.Add(21*time.Second)) {
		t.Fatalf("wakes %+v, want the third at now+delay", *got)
	}
}

// A dropped follow (daemon restart) or a refused one is followed again at the
// next listing.
func TestADroppedOrRefusedFollowIsFollowedAgain(t *testing.T) {
	w, ff, got, _ := rig()
	ws := []*model.Critic{cacheCritic("cache", map[string]string{"k": "@0000000000000000000000000000000a", "j": "@0000000000000000000000000000000b"})}
	ff.fail["@0000000000000000000000000000000b"] = true
	w.Sync(context.Background(), ws)
	if w.Following() != 1 {
		t.Fatalf("following %d, want 1 (one refused)", w.Following())
	}
	ff.drop("@0000000000000000000000000000000a")
	ff.fail["@0000000000000000000000000000000b"] = false
	w.Sync(context.Background(), ws)
	if w.Following() != 2 || ff.n != 3 {
		t.Fatalf("following %d after %d follows, want 2 after 3", w.Following(), ff.n)
	}
	ff.push("@0000000000000000000000000000000a", deltaTitle)
	ff.push("@0000000000000000000000000000000b", deltaTitle)
	if len(*got) != 2 {
		t.Fatalf("re-followed forms woke %d times, want 2", len(*got))
	}
}

func TestKeysTravelInTheContext(t *testing.T) {
	if Keys(context.Background()) != nil || Keys(With(context.Background(), nil)) != nil {
		t.Fatal("an ordinary round carries wake keys")
	}
	if got := Keys(With(context.Background(), []string{"a", "b"})); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("keys %q", got)
	}
}
