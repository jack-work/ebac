package bandstate

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) *Hub {
	t.Helper()
	h := &Hub{}
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 100 {
			t.Fatalf("fixture %s is %d bytes: an empty input proves nothing", name, len(b))
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	read("stats.json", &h.Stats)
	read("nodes.json", &h.Nodes)
	var ready, leased []Job
	read("ready.json", &ready)
	read("leased.json", &leased)
	h.Live = append(ready, leased...)
	if len(h.Stats.Queues) == 0 || len(h.Nodes) != 6 || len(h.Live) != 16 || h.Stats.Hub.Now == 0 {
		t.Fatalf("fixture shape: %d queues, %d nodes, %d live, now %d", len(h.Stats.Queues), len(h.Nodes), len(h.Live), h.Stats.Hub.Now)
	}
	return h
}

func has(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestRenderFixture(t *testing.T) {
	h := fixture(t)
	v := Render(h, []string{"leob", "ross"}, nil)
	nonEmpty := 0
	for _, q := range h.Stats.Queues {
		if q.Counts.Ready+q.Counts.Leased > 0 {
			nonEmpty++
		}
	}
	if nonEmpty == 0 || len(v.Queues) != nonEmpty {
		t.Fatalf("queues: %d lines for %d non-empty queues: %v", len(v.Queues), nonEmpty, v.Queues)
	}
	if !has(v.Queues, "gate.change ready=9 leased=2 oldest_ready_since=") || !has(v.Queues, "aether.decision ready=0 leased=3 oldest_ready_since=- ") {
		t.Fatalf("gate.change line: %v", v.Queues)
	}
	if len(v.Nodes) != 6 || !has(v.Nodes, "leob class=utility") || !has(v.Nodes, "barr class=heavy") || !has(v.Nodes, "running=4246") {
		t.Fatalf("nodes: %v", v.Nodes)
	}
	if len(v.Live) != 16 || !has(v.Live, "4246 aether.clippy-pr -1 barr/w3 ") || !has(v.Live, "4269 gate.change 0 - ") {
		t.Fatalf("live: %v", v.Live)
	}
	// No shared work in the fixture is the flake.gate/gate.change one in different queues: no dupe.
	if len(v.Dupes) != 0 {
		t.Fatalf("dupes in a clean fixture: %v", v.Dupes)
	}
	t.Logf("queues:\n%s\nnodes:\n%s\nlive[0:3]: %v", strings.Join(v.Queues, "\n"), strings.Join(v.Nodes, "\n"), v.Live[:3])
}

// The churn rule: the view renders instants, never ages, so re-polls a minute and an hour later (same UTC day)
// render the same view and nothing is written. Control: a new job changes it.
func TestRenderStableAcrossPolls(t *testing.T) {
	h := fixture(t)
	day := time.UnixMilli(h.Stats.Hub.Now).UTC()
	if day.Hour() > 22 {
		t.Fatalf("fixture hub now %s is too late in the day for a +1h same-day check", day)
	}
	js := func() string { b, _ := json.Marshal(Render(h, nil, nil)); return string(b) }
	a := js()
	h.Stats.Hub.Now += 60_000
	if b := js(); b != a {
		t.Fatalf("view changed after one minute:\n%s\n%s", a, b)
	}
	h.Stats.Hub.Now += 3600_000
	if c := js(); c != a {
		t.Fatalf("view changed an hour later with nothing new:\n%s\n%s", a, c)
	}
	h.Live = append(h.Live, clone(h.Live[0], 9100, map[string]string{"sha": "0000000000000000000000000000000000000009"}))
	if d := js(); d == a {
		t.Fatal("control: a new live job did not change the view")
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 11, 9, 22, 5, 0, 0, time.UTC).UnixMilli()
	for at, want := range map[time.Time]string{
		time.Date(2026, 11, 9, 21, 40, 59, 0, time.UTC): "21:40Z",
		time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC):  "11-08T23:59Z",
	} {
		if got := Since(at.UnixMilli(), now); got != want {
			t.Errorf("Since(%s) = %s; want %s", at, got, want)
		}
	}
}

func TestNodeStaleOnlyPastThreshold(t *testing.T) {
	h := fixture(t)
	h.Stats.Hub.NodeStaleS = 30
	h.Nodes[0].AgeS = 29
	if has(Render(h, nil, nil).Nodes, "stale") {
		t.Fatal("a fresh node shows stale")
	}
	h.Nodes[0].AgeS = 900
	h.Nodes[0].Received = h.Stats.Hub.Now - 900_000
	want := "stale_since=" + Since(h.Nodes[0].Received, h.Stats.Hub.Now)
	if !has(Render(h, nil, nil).Nodes, want) {
		t.Fatalf("a stale node does not say so: %v", Render(h, nil, nil).Nodes)
	}
}

func clone(j Job, id int64, params map[string]string) Job {
	c := j
	c.ID = id
	c.Payload.Params = map[string]json.RawMessage{}
	for k, v := range j.Payload.Params {
		c.Payload.Params[k] = v
	}
	for k, v := range params {
		c.Payload.Params[k], _ = json.Marshal(v)
	}
	return c
}

func find(t *testing.T, h *Hub, id int64) Job {
	t.Helper()
	for _, j := range h.Live {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no job %d in the fixture", id)
	return Job{}
}

// Same queue + sha + params with branch, label and why ignored is a dupe; any other param differing is not.
func TestDupes(t *testing.T) {
	h := fixture(t)
	g := find(t, h, 4269)
	h.Live = append(h.Live, clone(g, 9001, map[string]string{"label": "resubmitted", "why": "again"}))
	v := Render(h, nil, nil)
	if !has(v.Dupes, "dupe gate.change 5f4ddf1b: 4269,9001") {
		t.Fatalf("label/why resubmit not a dupe: %v", v.Dupes)
	}
	h.Live[len(h.Live)-1] = clone(g, 9001, map[string]string{"sha": "0000000000000000000000000000000000000001"})
	if v := Render(h, nil, nil); len(v.Dupes) != 0 {
		t.Fatalf("control: a different sha is a dupe: %v", v.Dupes)
	}
	b := find(t, h, 4246)
	h.Live[len(h.Live)-1] = clone(b, 9002, map[string]string{"branch": "other/branch"})
	if v := Render(h, nil, nil); !has(v.Dupes, "dupe aether.clippy-pr 1c93d0cb: 4246,9002") {
		t.Fatalf("branch must be ignored: %v", v.Dupes)
	}
}

func TestSuperseded(t *testing.T) {
	h := fixture(t)
	br := "jokellih/stack/effort-egress/effort-reco" // job 4246's branch prefix; read the real one
	for _, b := range Branches(h.Live) {
		if strings.HasPrefix(b, br) {
			br = b
		}
	}
	if got := Branches(h.Live); len(got) != 1 || got[0] != br {
		t.Fatalf("branches to check = %v; want only job 4246's (main is trunk, never checked)", got)
	}
	if v := Render(h, nil, map[string]string{br: "1c93d0cbdb78231be750c9e188777b9b47124a9d"}); len(v.Dupes) != 0 {
		t.Fatalf("tip == sha flagged: %v", v.Dupes)
	}
	if v := Render(h, nil, map[string]string{br: "ffffffffffffffffffffffffffffffffffffffff"}); !has(v.Dupes, "superseded 4246 aether.clippy-pr") || !has(v.Dupes, "tip=ffffffff") {
		t.Fatalf("moved tip not flagged: %v", v.Dupes)
	}
	if v := Render(h, nil, map[string]string{br: ""}); !has(v.Dupes, "tip=gone") {
		t.Fatalf("deleted branch not flagged: %v", v.Dupes)
	}
	// main moving under a pinned eval is not supersession.
	if v := Render(h, nil, map[string]string{"main": "ffffffffffffffffffffffffffffffffffffffff"}); has(v.Dupes, "superseded") {
		t.Fatalf("trunk flagged: %v", v.Dupes)
	}
}

func TestLiveCap(t *testing.T) {
	h := fixture(t)
	base := h.Live[0]
	for i := 0; i < 100; i++ {
		h.Live = append(h.Live, clone(base, int64(10000+i), map[string]string{"sha": strings.Repeat("a", 39) + string(rune('0'+i%10))}))
	}
	v := Render(h, nil, nil)
	if len(v.Live) != LiveCap+1 || v.Live[LiveCap] != "+56 more" {
		t.Fatalf("cap: %d lines, last %q", len(v.Live), v.Live[len(v.Live)-1])
	}
}

// ebac never reads a log-bearing field: a hub that still sends diagnostics, a summary, a result or a reason (the meta
// view strips them; this is the belt) never reaches the rendered form.
func TestNeverRendersLogFields(t *testing.T) {
	const secret = "LOGLINE-token=hunter2"
	raw := `[{"id":1,"queue":"q","state":"ready","priority":0,"created":1,"payload":{"template":"t","params":{"sha":"abc"}},` +
		`"diagnostics":["` + secret + `"],"summary":"` + secret + `","result":{"x":"` + secret + `"},"term":{"last_reason":"` + secret + `"}}]`
	if !strings.Contains(raw, secret) {
		t.Fatal("control: the input carries the secret")
	}
	var jobs []Job
	if err := json.Unmarshal([]byte(raw), &jobs); err != nil {
		t.Fatal(err)
	}
	h := fixture(t)
	h.Live = jobs
	out, _ := json.Marshal(Render(h, nil, nil))
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("a log field reached the view: %s", out)
	}
	if !strings.Contains(string(out), "1 q 0 - ") {
		t.Fatalf("control: the job itself was not rendered: %s", out)
	}
}

// The client reads only /stats, /nodes and the /jobs list over the socket, and surfaces a refusal's status and text.
func TestClientPathsAndRefusal(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var paths []string
	deny := false
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.Query().Get("state"))
		d := deny
		mu.Unlock()
		if d {
			http.Error(w, "uid 988 is not a hub principal", http.StatusForbidden)
			return
		}
		name := map[string]string{"/stats": "stats.json", "/nodes": "nodes.json"}[r.URL.Path]
		if r.URL.Path == "/jobs" {
			name = r.URL.Query().Get("state") + ".json"
		}
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Write(b)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	h, err := (&Client{Socket: sock}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Live) != 16 || len(h.Nodes) != 6 {
		t.Fatalf("fetched %d live, %d nodes", len(h.Live), len(h.Nodes))
	}
	want := []string{"/stats?", "/nodes?", "/jobs?ready", "/jobs?leased"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("paths %v; want exactly %v", paths, want)
	}
	mu.Lock()
	deny = true
	mu.Unlock()
	_, err = (&Client{Socket: sock}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "not a hub principal") {
		t.Fatalf("refusal not surfaced with its text: %v", err)
	}
}

// ---------- Step: write only on change, tip cadence ----------

func TestStepWritesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	r := Step(ctx, State{}, h, Config{}, now, nil)
	if strings.Join(r.Changed, ",") != "queues,nodes,live,dupes" || len(r.Set) != 5 || r.Set["updated"] == nil {
		t.Fatalf("first round: changed %v, %d keys", r.Changed, len(r.Set))
	}
	h.Stats.Hub.Now += 60_000
	r2 := Step(ctx, r.State, h, Config{}, now.Add(time.Minute), nil)
	if len(r2.Set) != 0 {
		t.Fatalf("a quiet re-poll wrote %v", r2.Changed)
	}
	h.Live = append(h.Live, clone(h.Live[0], 9200, map[string]string{"sha": "0000000000000000000000000000000000000007"}))
	r3 := Step(ctx, r2.State, h, Config{}, now.Add(2*time.Minute), nil)
	if strings.Join(r3.Changed, ",") != "live" || r3.Set["updated"] == nil || len(r3.Set) != 2 {
		t.Fatalf("a new job: changed %v (%d keys); want live + updated", r3.Changed, len(r3.Set))
	}
}

func TestStepTipCadenceAndFailure(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	calls := 0
	fail := false
	tip := func(_ context.Context, repo, branch string) (string, error) {
		calls++
		if repo != DefaultRepo {
			t.Fatalf("repo %q", repo)
		}
		if fail {
			return "", fmt.Errorf("HTTP 502")
		}
		return "ffffffffffffffffffffffffffffffffffffffff", nil
	}
	r := Step(ctx, State{}, h, Config{}, now, tip)
	if calls != 1 || !has(r.State.View.Dupes, "superseded 4246") {
		t.Fatalf("first round: %d calls, dupes %v", calls, r.State.View.Dupes)
	}
	r = Step(ctx, r.State, h, Config{}, now.Add(4*time.Minute), tip)
	if calls != 1 {
		t.Fatalf("tips asked again within %s: %d calls", TipEvery, calls)
	}
	fail = true
	r = Step(ctx, r.State, h, Config{}, now.Add(6*time.Minute), tip)
	if calls != 2 || r.State.TipErr == "" || !has(r.State.View.Dupes, "superseded 4246") {
		t.Fatalf("a failed refresh must keep the old tips and say so: calls %d err %q dupes %v", calls, r.State.TipErr, r.State.View.Dupes)
	}
}

// The hub's metaReaders view may carry no lease owner and no priority: the node comes from the nodes' reports, the
// priority renders "-".
func TestMetaViewWithoutLeaseOwnerOrPriority(t *testing.T) {
	h := fixture(t)
	for i := range h.Live {
		h.Live[i].LeaseOwner, h.Live[i].Priority = nil, nil
	}
	v := Render(h, nil, nil)
	if !has(v.Live, "4246 aether.clippy-pr - barr ") || !has(v.Live, "4269 gate.change - - ") {
		t.Fatalf("meta view: %v", v.Live)
	}
}
