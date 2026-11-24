// Package wake runs a GitHub-cache entity's write within seconds of a seat
// changing its form (augustine A3, write-on-change). ebacd follows every
// entity form of each github-cache critic (figaro.follow on the form's hub:
// every committed patch arrives as a form.delta push). A delta touching a
// write field (model.GHCacheWakes) wakes the critic's scheduler for that
// entity, debounced: the first change of a burst is written Delay after it,
// and one entity is written at most once per Gap. Reads keep their cadence:
// the woken round is write-only (plugin.Request.Wake).
package wake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// Defaults (contract 5: written within 2 s, one write per entity per 5 s).
// The scheduler ticks every second, so Delay 1 s writes within 2 s.
const (
	DefaultDelay = time.Second
	DefaultGap   = 5 * time.Second
)

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
// figaro form patch tree). It returns when the follow is up; done closes when
// it drops (the next Sync follows again); stop ends it.
type Follow func(ctx context.Context, formID string, on func(patch json.RawMessage)) (stop func(), done <-chan struct{}, err error)

// Watcher keeps one follow per entity form of every github-cache critic.
type Watcher struct {
	Follow Follow
	// Wake schedules critic's write-only round for the entity key at at
	// (sched.Scheduler.Wake).
	Wake  func(critic, key string, at time.Time)
	Now   func() time.Time
	Delay time.Duration
	Gap   time.Duration
	Out   io.Writer

	mu      sync.Mutex
	follows map[string]*followed // by form id
	next    map[string]time.Time // critic/key -> the wake last scheduled
}

type followed struct {
	critic, key string
	stop        func()
	done        <-chan struct{}
}

func (w *Watcher) defaults() {
	if w.Delay <= 0 {
		w.Delay = DefaultDelay
	}
	if w.Gap <= 0 {
		w.Gap = DefaultGap
	}
	if w.Out == nil {
		w.Out = io.Discard
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.follows == nil {
		w.follows, w.next = map[string]*followed{}, map[string]time.Time{}
	}
}

// Sync makes the follows match the critics' entity forms: a new child is
// followed, a retired one or a stopped critic's dropped, a follow that
// dropped followed again. It is called on every critic listing; a failed
// follow is retried at the next.
func (w *Watcher) Sync(ctx context.Context, ws []*model.Critic) {
	w.mu.Lock()
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
	var start []string
	for id, f := range w.follows {
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
	for id := range want {
		if w.follows[id] == nil {
			start = append(start, id)
		}
	}
	live := map[string]bool{}
	for _, t := range want {
		live[t.critic+"/"+t.key] = true
	}
	for k := range w.next {
		if !live[k] {
			delete(w.next, k)
		}
	}
	w.mu.Unlock()
	sort.Strings(start)
	for _, id := range start {
		t := want[id]
		stop, done, err := w.Follow(ctx, id, func(patch json.RawMessage) { w.delta(t.critic, t.key, patch) })
		if err != nil {
			fmt.Fprintf(w.Out, "wake: follow %s (%s/%s): %v (retried at the next listing)\n", id, t.critic, t.key, err)
			continue
		}
		w.mu.Lock()
		if old := w.follows[id]; old != nil {
			old.stop()
		}
		w.follows[id] = &followed{critic: t.critic, key: t.key, stop: stop, done: done}
		w.mu.Unlock()
	}
}

// Following is how many forms are followed (the heartbeat; tests).
func (w *Watcher) Following() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.follows)
}

// Close stops every follow.
func (w *Watcher) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, f := range w.follows {
		f.stop()
		delete(w.follows, id)
	}
}

func (w *Watcher) delta(critic, key string, patch json.RawMessage) {
	paths, err := Paths(patch)
	if err != nil {
		fmt.Fprintf(w.Out, "wake: %s/%s: unreadable form delta: %v\n", critic, key, err)
		return
	}
	hit := ""
	for _, p := range paths {
		if model.GHCacheWakes(p) {
			hit = p
			break
		}
	}
	if hit == "" {
		return
	}
	w.mu.Lock()
	w.defaults()
	now := w.Now()
	id := critic + "/" + key
	last := w.next[id]
	if last.After(now) {
		w.mu.Unlock()
		return // a wake is already scheduled: this change rides it
	}
	at := now.Add(w.Delay)
	if g := last.Add(w.Gap); g.After(at) {
		at = g
	}
	w.next[id] = at
	w.mu.Unlock()
	fmt.Fprintf(w.Out, "wake: %s/%s: %s changed: write at %s\n", critic, key, hit, at.UTC().Format("15:04:05.000"))
	w.Wake(critic, key, at)
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
