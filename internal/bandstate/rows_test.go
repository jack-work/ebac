package bandstate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jack-work/figaro/api/form"
)

// boardAfter applies a round's keys to a form the way the core's Commit does
// (top-level SetPath) and returns the patch a studier would receive.
func boardAfter(t *testing.T, cur form.Snapshot, set map[string]json.RawMessage) (form.Snapshot, form.Patch) {
	t.Helper()
	next := cur
	for k, v := range set {
		next = next.SetPath(k, v)
	}
	return next, next.Diff(cur)
}

// leafPaths lists every path a patch touches, dotted.
func leafPaths(p form.Patch) []string {
	var out []string
	for _, e := range p.Entries() {
		out = append(out, e.Key)
	}
	return out
}

// delivered is what a studier receives: the new value at each changed path.
func delivered(p form.Patch) int {
	n := 0
	for _, e := range p.Entries() {
		n += len(e.Key) + len(e.New)
	}
	return n
}

// One queue's change patches ONE leaf (queues.<name>), not the whole table; the positive control is the old array
// shape, where the same change re-sends every queue line.
func TestStepRowsPatchOneLeaf(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r1 := Step(ctx, State{}, h, Config{}, now, nil, nil)
	var q map[string]string
	if err := json.Unmarshal(r1.Set["queues"], &q); err != nil || len(q) == 0 {
		t.Fatalf("queues is not an object keyed by queue: %s (%v)", r1.Set["queues"], err)
	}
	for k, line := range q {
		if strings.Contains(k, ".") || !strings.HasPrefix(line, strings.ReplaceAll(k, "_", ".")+" ") {
			t.Fatalf("row key %q / line %q: a member key must carry no dot and the line its real name", k, line)
		}
	}
	var n map[string]string
	if err := json.Unmarshal(r1.Set["nodes"], &n); err != nil || len(n) != 6 || n["leob"] == "" {
		t.Fatalf("nodes is not an object keyed by node: %s (%v)", r1.Set["nodes"], err)
	}
	board, _ := boardAfter(t, form.FromMap(map[string]json.RawMessage{}), r1.Set)

	j := clone(readyJob(t, h), 9200, map[string]string{"sha": "0000000000000000000000000000000000000007"})
	j.Created = h.Stats.Hub.Now
	h.Live = append(h.Live, j)
	bump(h, j.Queue)
	h.Stats.Hub.Now += 60_000
	r2 := Step(ctx, r1.State, h, Config{}, now.Add(time.Minute), nil, nil)
	_, p := boardAfter(t, board, r2.Set)
	got := delivered(p)
	t.Logf("one queue changed: %d bytes delivered, entries %v", got, leafPaths(p))
	if len(leafPaths(p)) != 1 {
		t.Fatalf("one queue's change touched %v; want one leaf", leafPaths(p))
	}

	// Positive control: the same two views written as arrays (the old shape).
	old1 := map[string]json.RawMessage{"queues": mustJSON(r1.State.View.Queues)}
	oldBoard, _ := boardAfter(t, form.FromMap(map[string]json.RawMessage{}), old1)
	_, op := boardAfter(t, oldBoard, map[string]json.RawMessage{"queues": mustJSON(r2.State.View.Queues)})
	ctl := delivered(op)
	t.Logf("control, array shape: %d bytes delivered", ctl)
	if len(q) < 3 || got*2 > ctl {
		t.Fatalf("one queue's change: %d bytes as rows vs %d as an array (%d queues): rows must cost under half", got, ctl, len(q))
	}
}

// updated is a heartbeat: written on the first round and then at most every UpdatedEvery, even when queues change
// every minute; a quiet hub still gets it once UpdatedEvery has passed.
func TestStepUpdatedIsAHeartbeat(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r := Step(ctx, State{}, h, Config{}, now, nil, nil)
	if r.Set["updated"] == nil {
		t.Fatal("first round wrote no updated")
	}
	st := r.State
	writes := 0
	for i := 1; i <= 9; i++ { // a change every minute for 9 minutes
		j := clone(readyJob(t, h), int64(9300+i), map[string]string{"sha": "000000000000000000000000000000000000000" + string(rune('0'+i))})
		h.Live = append(h.Live, j)
		bump(h, j.Queue)
		h.Stats.Hub.Now += 60_000
		ri := Step(ctx, st, h, Config{}, now.Add(time.Duration(i)*time.Minute), nil, nil)
		if len(ri.Changed) == 0 {
			t.Fatalf("minute %d: the control change was not seen", i)
		}
		if ri.Set["updated"] != nil {
			writes++
		}
		st = ri.State
	}
	if writes != 0 {
		t.Fatalf("updated written %d times in 9 changing minutes; want 0 (heartbeat every %v)", writes, UpdatedEvery)
	}
	h.Stats.Hub.Now += 60_000 // minute 10, quiet
	r10 := Step(ctx, st, h, Config{}, now.Add(10*time.Minute), nil, nil)
	if r10.Set["updated"] == nil || len(r10.Changed) != 0 {
		t.Fatalf("minute 10 (quiet): updated %s, changed %v; want the heartbeat alone", r10.Set["updated"], r10.Changed)
	}
}

// A state saved by the array-shaped mirror rewrites queues and nodes once as objects, then is quiet.
func TestStepMigratesArraysOnce(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r := Step(ctx, State{}, h, Config{}, now, nil, nil)
	old := r.State
	old.Rows = false // as the array-shaped version left it
	h.Stats.Hub.Now += 60_000
	m := Step(ctx, old, h, Config{}, now.Add(time.Minute), nil, nil)
	if m.Set["queues"] == nil || m.Set["nodes"] == nil {
		t.Fatalf("migration did not rewrite the tables: %v", m.Changed)
	}
	h.Stats.Hub.Now += 60_000
	q := Step(ctx, m.State, h, Config{}, now.Add(2*time.Minute), nil, nil)
	if len(q.Changed) != 0 {
		t.Fatalf("after migration a quiet hub wrote %v", q.Changed)
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
