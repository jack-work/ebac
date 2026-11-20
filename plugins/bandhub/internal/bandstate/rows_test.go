package bandstate

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jack-work/figaro/api/form"
)

// boardAfter applies a round's keys to a form the way the core's Commit does (top-level SetPath) and returns the
// patch a studier would receive.
func boardAfter(t *testing.T, cur form.Snapshot, set map[string]json.RawMessage) (form.Snapshot, form.Patch) {
	t.Helper()
	next := cur
	for k, v := range set {
		next = next.SetPath(k, v)
	}
	return next, next.Diff(cur)
}

func leafPaths(p form.Patch) []string {
	var out []string
	for _, e := range p.Entries() {
		out = append(out, e.Key)
	}
	sort.Strings(out)
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

// Shapes: queues, nodes and node_load are objects keyed by name (dots as _, the real name first in the value);
// nodes carries what a node IS, node_load what moves with every job.
func TestRowShapes(t *testing.T) {
	h := fixture(t)
	r := Step(context.Background(), State{}, h, Config{}, time.UnixMilli(h.Stats.Hub.Now), nil, nil)
	for _, k := range []string{"queues", "nodes", "node_load"} {
		var m map[string]string
		if err := json.Unmarshal(r.Set[k], &m); err != nil || len(m) == 0 {
			t.Fatalf("%s is not an object keyed by name: %s (%v)", k, r.Set[k], err)
		}
		for key, line := range m {
			if strings.Contains(key, ".") || !strings.HasPrefix(line, strings.ReplaceAll(key, "_", ".")+" ") {
				t.Fatalf("%s row %q / %q: a member key carries no dot and the line starts with the real name", k, key, line)
			}
		}
	}
	var nodes, load map[string]string
	json.Unmarshal(r.Set["nodes"], &nodes)
	json.Unmarshal(r.Set["node_load"], &load)
	if !strings.Contains(nodes["leob"], "class=") || strings.Contains(nodes["leob"], "free_slots") || strings.Contains(nodes["leob"], "disk_free") {
		t.Fatalf("nodes row is not what a node IS: %q", nodes["leob"])
	}
	if !strings.Contains(load["leob"], "free_slots=") || !strings.Contains(load["leob"], "disk_free=") || strings.Contains(load["leob"], "class=") {
		t.Fatalf("node_load row is not what moves: %q", load["leob"])
	}
}

// At the heartbeat, one queue's change patches ONE leaf (queues.<name>), not the whole table. Positive control: the
// same two views written as the old array re-send every queue line.
func TestHeartbeatPatchesOneQueueLeaf(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r1 := Step(ctx, State{}, h, Config{}, now, nil, nil)
	board, _ := boardAfter(t, form.FromMap(map[string]json.RawMessage{}), r1.Set)
	j := clone(readyJob(t, h), 9200, map[string]string{"sha": "0000000000000000000000000000000000000007"})
	j.Created = h.Stats.Hub.Now
	h.Live = append(h.Live, j)
	bump(h, j.Queue)
	h.Stats.Hub.Now += int64(UpdatedEvery / time.Millisecond)
	r2 := Step(ctx, r1.State, h, Config{}, now.Add(UpdatedEvery), nil, nil)
	_, p := boardAfter(t, board, r2.Set)
	var qpaths []string
	for _, k := range leafPaths(p) {
		if strings.HasPrefix(k, "queues.") {
			qpaths = append(qpaths, k)
		}
	}
	if len(qpaths) != 1 {
		t.Fatalf("one queue's change touched %v; want one leaf", qpaths)
	}
	_, onlyQ := boardAfter(t, board, map[string]json.RawMessage{"queues": r2.Set["queues"]})
	got := delivered(onlyQ)
	oldBoard, _ := boardAfter(t, form.FromMap(map[string]json.RawMessage{}), map[string]json.RawMessage{"queues": mustJSON(r1.State.View.Queues)})
	_, op := boardAfter(t, oldBoard, map[string]json.RawMessage{"queues": mustJSON(r2.State.View.Queues)})
	ctl := delivered(op)
	t.Logf("one queue changed: %d bytes as rows, %d as the old array", got, ctl)
	if got*2 > ctl {
		t.Fatalf("rows (%d) not under half the array (%d)", got, ctl)
	}
}

// Between heartbeats load writes nothing, a change to what a node IS (it goes stale) is written at once, and the
// heartbeat comes every UpdatedEvery even on a quiet hub. Positive control: the same load change IS written at the
// heartbeat, so "nothing between heartbeats" is not a blind instrument.
func TestLoadWaitsForTheHeartbeat(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	st := Step(ctx, State{}, h, Config{}, now, nil, nil).State
	for i := 1; i <= 9; i++ { // a load change every minute
		j := clone(readyJob(t, h), int64(9300+i), map[string]string{"sha": "000000000000000000000000000000000000000" + string(rune('0'+i))})
		h.Live = append(h.Live, j)
		bump(h, j.Queue)
		h.Stats.Hub.Now += 60_000
		ri := Step(ctx, st, h, Config{}, now.Add(time.Duration(i)*time.Minute), nil, nil)
		if len(ri.Set) != 0 {
			t.Fatalf("minute %d: load between heartbeats wrote %v", i, ri.Changed)
		}
		st = ri.State
	}
	// What a node IS changes: written at once.
	h.Nodes[0].AgeS = 900
	h.Nodes[0].Received = h.Stats.Hub.Now - 900_000
	h.Stats.Hub.NodeStaleS = 300
	rs := Step(ctx, st, h, Config{}, now.Add(9*time.Minute+30*time.Second), nil, nil)
	if strings.Join(rs.Changed, ",") != "nodes" {
		t.Fatalf("a node going stale: changed %v; want nodes at once", rs.Changed)
	}
	st = rs.State
	h.Stats.Hub.Now += 60_000 // minute 10: the heartbeat, carrying the 9 minutes of load
	r10 := Step(ctx, st, h, Config{}, now.Add(10*time.Minute), nil, nil)
	if strings.Join(r10.Changed, ",") != "queues,node_load" || r10.Set["updated"] == nil {
		t.Fatalf("minute 10: changed %v updated %s; want the heartbeat", r10.Changed, r10.Set["updated"])
	}
}

// A state saved by the array-shaped mirror rewrites every key once in the new shape, then is quiet.
func TestStepMigratesArraysOnce(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r := Step(ctx, State{}, h, Config{}, now, nil, nil)
	old := r.State
	old.Rows = false // as the array-shaped version left it
	h.Stats.Hub.Now += 60_000
	m := Step(ctx, old, h, Config{}, now.Add(time.Minute), nil, nil)
	if m.Set["queues"] == nil || m.Set["nodes"] == nil || m.Set["node_load"] == nil {
		t.Fatalf("migration did not rewrite the tables: %v", m.Changed)
	}
	h.Stats.Hub.Now += 60_000
	if q := Step(ctx, m.State, h, Config{}, now.Add(2*time.Minute), nil, nil); len(q.Changed) != 0 {
		t.Fatalf("after migration a quiet hub wrote %v", q.Changed)
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// The cadence is configurable (load_every_sec): at 30 min, minute 10 writes no load; minute 30 does.
func TestLoadEveryConfigured(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	cfg := Config{LoadEvery: 30 * time.Minute}
	st := Step(ctx, State{}, h, cfg, now, nil, nil).State
	j := clone(readyJob(t, h), 9400, map[string]string{"sha": "0000000000000000000000000000000000000009"})
	h.Live = append(h.Live, j)
	bump(h, j.Queue)
	h.Stats.Hub.Now += int64(10 * time.Minute / time.Millisecond)
	r10 := Step(ctx, st, h, cfg, now.Add(10*time.Minute), nil, nil)
	if len(r10.Set) != 0 {
		t.Fatalf("minute 10 at a 30-min cadence wrote %v", r10.Changed)
	}
	h.Stats.Hub.Now += int64(20 * time.Minute / time.Millisecond)
	if r30 := Step(ctx, r10.State, h, cfg, now.Add(30*time.Minute), nil, nil); r30.Set["queues"] == nil {
		t.Fatalf("minute 30 at a 30-min cadence wrote %v; want the heartbeat", r30.Changed)
	}
}
