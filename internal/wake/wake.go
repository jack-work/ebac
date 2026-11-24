// Package wake runs a GitHub-cache entity's write within seconds of a seat
// changing its form (augustine A3, write-on-change). ebacd follows every
// entity form of each github-cache critic (figaro.follow on the form's hub:
// every committed patch arrives as a form.delta push). A delta touching a
// write field (model.GHCacheWakes) wakes the critic's scheduler for that
// entity; the scheduler debounces (sched.Scheduler.Wake). Reads keep their
// cadence: the woken round is write-only (plugin.Request.Wake).
package wake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/gluck/ebac/sdk/model"
)

// DefaultParallel bounds the follows dialled at once (review B2).
const DefaultParallel = 8

type ctxKey struct{}

// With carries a write-only round's entity keys to the round (the scheduler
// sets it, the poller reads it).
func With(ctx context.Context, keys []string) context.Context {
	if len(keys) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, append([]string(nil), keys...))
}

// Keys is the round's wake keys; nil for an ordinary round.
func Keys(ctx context.Context) []string {
	k, _ := ctx.Value(ctxKey{}).([]string)
	return k
}

// Follow follows one form: on receives each committed patch as JSON (a
// figaro form patch tree). It returns when the follow is up or ctx ends;
// done closes when it drops (the next Sync follows again); stop ends it.
type Follow func(ctx context.Context, formID string, on func(patch json.RawMessage)) (stop func(), done <-chan struct{}, err error)

// Watcher keeps one follow per entity form of every github-cache critic and
// reports each change of a write field to Wake. It keeps no deadlines: the
// scheduler alone decides when a reported entity is written (review B1).
type Watcher struct {
	Follow Follow
	// Wake reports that a seat changed critic's entity key
	// (sched.Scheduler.Wake).
	Wake func(critic, key string)
	// Parallel bounds the follows dialled at once.
	Parallel int
	Out      io.Writer

	mu      sync.Mutex
	follows map[string]*followed // by form id: up, or being dialled (stop nil)
	sem     chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type followed struct {
	critic, key string
	stop        func()
	done        <-chan struct{}
}

func (w *Watcher) defaults() {
	if w.Parallel <= 0 {
		w.Parallel = DefaultParallel
	}
	if w.Out == nil {
		w.Out = io.Discard
	}
	if w.follows == nil {
		w.follows = map[string]*followed{}
		w.sem = make(chan struct{}, w.Parallel)
		w.ctx, w.cancel = context.WithCancel(context.Background())
	}
}

// Sync makes the follows match the critics' entity forms: a new child is
// followed, a retired one or a stopped critic's dropped (its connection
// closed), a follow that dropped followed again. It never blocks on a dial:
// follows are dialled in the background, at most Parallel at once, and a
// refused one is retried at the next listing. It is called on every critic
// listing.
func (w *Watcher) Sync(ctx context.Context, ws []*model.Critic) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaults()
	want := map[string]followed{}
	for _, c := range ws {
		if c.SourceName() != model.SourceGHCache || c.Runtime.Stopped || c.Runtime.Archived || c.NoAuthority {
			continue
		}
		for key, id := range c.Runtime.Children {
			if id != "" {
				want[id] = followed{critic: c.Name, key: key}
			}
		}
	}
	for id, f := range w.follows {
		if f.stop == nil {
			continue // still dialling: its goroutine settles it
		}
		dropped := false
		select {
		case <-f.done:
			dropped = true
		default:
		}
		if t, ok := want[id]; !ok || dropped || t.critic != f.critic || t.key != f.key {
			f.stop()
			delete(w.follows, id)
			if dropped && ok {
				fmt.Fprintf(w.Out, "wake: follow of %s (%s/%s) dropped: following again\n", id, f.critic, f.key)
			}
		}
	}
	var start []string
	for id := range want {
		if w.follows[id] == nil {
			start = append(start, id)
		}
	}
	sort.Strings(start)
	for _, id := range start {
		t := want[id]
		w.follows[id] = &followed{critic: t.critic, key: t.key}
		w.wg.Add(1)
		go w.dial(id, t)
	}
}

// dial follows one form in the background, holding one of Parallel slots.
func (w *Watcher) dial(id string, t followed) {
	defer w.wg.Done()
	select {
	case w.sem <- struct{}{}:
	case <-w.ctx.Done():
		w.settle(id, nil, nil)
		return
	}
	stop, done, err := w.Follow(w.ctx, id, func(patch json.RawMessage) { w.delta(t.critic, t.key, patch) })
	<-w.sem
	if err != nil {
		fmt.Fprintf(w.Out, "wake: follow %s (%s/%s): %v (retried at the next listing)\n", id, t.critic, t.key, err)
		w.settle(id, nil, nil)
		return
	}
	w.settle(id, stop, done)
}

// settle records a dial's outcome: up (stop set), or gone (retried next Sync).
func (w *Watcher) settle(id string, stop func(), done <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.follows[id]
	if stop == nil || f == nil || w.ctx.Err() != nil {
		if f != nil && f.stop == nil {
			delete(w.follows, id)
		}
		if stop != nil {
			stop()
		}
		return
	}
	f.stop, f.done = stop, done
}

// Following is how many forms are followed and up (the heartbeat; tests).
func (w *Watcher) Following() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, f := range w.follows {
		if f.stop != nil {
			n++
		}
	}
	return n
}

// Close cancels the dials in flight, waits for them, and stops every follow.
func (w *Watcher) Close() {
	w.mu.Lock()
	w.defaults()
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, f := range w.follows {
		if f.stop != nil {
			f.stop()
		}
		delete(w.follows, id)
	}
}

func (w *Watcher) delta(critic, key string, patch json.RawMessage) {
	paths, err := Paths(patch)
	if err != nil {
		fmt.Fprintf(w.Out, "wake: %s/%s: unreadable form delta: %v\n", critic, key, err)
		return
	}
	for _, p := range paths {
		if model.GHCacheWakes(p) {
			w.Wake(critic, key)
			return
		}
	}
}

// patch is figaro's form patch tree (api/form.Patch), read structurally so
// the core does not pin figaro's form package version.
type patch struct {
	Scalar json.RawMessage `json:"scalar,omitempty"`
	Object *struct {
		Set    map[string]json.RawMessage `json:"Set,omitempty"`
		Delete map[string]json.RawMessage `json:"Delete,omitempty"`
		Update map[string]patch           `json:"Update,omitempty"`
	} `json:"object,omitempty"`
	List json.RawMessage `json:"list,omitempty"`
}

// Paths are the dot paths a form patch changes, in order: every key set,
// deleted or updated, down to the deepest object level named (a key set to
// an object is one path; its leaves are below it).
func Paths(raw json.RawMessage) ([]string, error) {
	var d struct {
		Patch json.RawMessage `json:"patch"`
	}
	if err := json.Unmarshal(raw, &d); err == nil && len(d.Patch) > 0 {
		raw = d.Patch // a whole form.delta push
	}
	var p patch
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	var out []string
	var walk func(q patch, prefix []string)
	walk = func(q patch, prefix []string) {
		at := strings.Join(prefix, ".")
		if q.Object == nil {
			if at != "" {
				out = append(out, at)
			}
			return
		}
		for _, m := range []map[string]json.RawMessage{q.Object.Set, q.Object.Delete} {
			for _, k := range sortedKeys(m) {
				out = append(out, strings.Join(append(prefix, k), "."))
			}
		}
		keys := make([]string, 0, len(q.Object.Update))
		for k := range q.Object.Update {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(q.Object.Update[k], append(append([]string(nil), prefix...), k))
		}
	}
	walk(p, nil)
	return out, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
