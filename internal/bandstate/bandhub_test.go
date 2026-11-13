package bandstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	v := Render(h, []string{"leob", "ross"}, nil, nil)
	nonEmpty := 0
	for _, q := range h.Stats.Queues {
		if q.Counts.Ready+q.Counts.Leased > 0 {
			nonEmpty++
		}
	}
	if nonEmpty == 0 || len(v.Queues) != nonEmpty {
		t.Fatalf("queues: %d lines for %d non-empty queues: %v", len(v.Queues), nonEmpty, v.Queues)
	}
	if !has(v.Queues, "gate.change ready=9 leased=2 oldest_ready=") || !has(v.Queues, "aether.decision ready=0 leased=3 oldest_ready=-") || !has(v.Queues, "@") {
		t.Fatalf("gate.change line: %v", v.Queues)
	}
	if len(v.Nodes) != 6 || !has(v.Nodes, "leob class=utility") || !has(v.Nodes, "barr class=heavy") || !has(v.Nodes, "running=4246") {
		t.Fatalf("nodes: %v", v.Nodes)
	}
	// No shared work in the fixture is the flake.gate/gate.change one in different queues: no dupe.
	if len(v.Dupes) != 0 {
		t.Fatalf("dupes in a clean fixture: %v", v.Dupes)
	}
	t.Logf("queues:\n%s\nnodes:\n%s", strings.Join(v.Queues, "\n"), strings.Join(v.Nodes, "\n"))
}

// The churn rule: the view renders instants, never ages, so re-polls a minute and an hour later (same UTC day)
// render the same view and nothing is written. Control: a new job changes it.
func TestRenderStableAcrossPolls(t *testing.T) {
	h := fixture(t)
	day := time.UnixMilli(h.Stats.Hub.Now).UTC()
	if day.Hour() > 22 {
		t.Fatalf("fixture hub now %s is too late in the day for a +1h same-day check", day)
	}
	js := func() string { b, _ := json.Marshal(Render(h, nil, nil, nil)); return string(b) }
	a := js()
	h.Stats.Hub.Now += 60_000
	if b := js(); b != a {
		t.Fatalf("view changed after one minute:\n%s\n%s", a, b)
	}
	h.Stats.Hub.Now += 3600_000
	if c := js(); c != a {
		t.Fatalf("view changed an hour later with nothing new:\n%s\n%s", a, c)
	}
	// Control: a ready job queued before every other one becomes its queue's oldest.
	old := clone(readyJob(t, h), 9100, map[string]string{"sha": "0000000000000000000000000000000000000009"})
	old.Created = 0 // queued before every other job
	h.Live = append(h.Live, old)
	if d := js(); d == a {
		t.Fatal("control: a new oldest ready job did not change the view")
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
	if has(Render(h, nil, nil, nil).Nodes, "stale") {
		t.Fatal("a fresh node shows stale")
	}
	h.Nodes[0].AgeS = 900
	h.Nodes[0].Received = h.Stats.Hub.Now - 900_000
	want := "stale_since=" + Since(h.Nodes[0].Received, h.Stats.Hub.Now)
	if !has(Render(h, nil, nil, nil).Nodes, want) {
		t.Fatalf("a stale node does not say so: %v", Render(h, nil, nil, nil).Nodes)
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

// hubSum is what the hub sends as params_sum: a digest of the FULL params, canonical (sorted keys).
func hubSum(full map[string]string) string {
	b, _ := json.Marshal(full) // Go marshals map keys sorted
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// flakeCheck is a flake.check job as ebac's metadata view sees it: attrs hidden, only the hub's digest carries them.
func flakeCheck(id int64, full map[string]string) Job {
	var j Job
	j.ID, j.Queue, j.State, j.Created = id, "flake.check", "leased", 1
	j.Payload.Template = "flake.check"
	j.Payload.Params = map[string]json.RawMessage{}
	for _, k := range []string{"branch", "sha", "label", "node"} {
		if v, ok := full[k]; ok {
			j.Payload.Params[k], _ = json.Marshal(v)
		}
	}
	j.Payload.ParamsSum = hubSum(full)
	return j
}

// Dupes key on queue + template + the FULL params (the hub's params_sum). flake.check 4709/4710 (same sha, attrs ci
// vs keel-web-*) are NOT dupes; identical params are; a row without params_sum is never grouped.
func TestDupes(t *testing.T) {
	h := fixture(t)
	sha := "0d1247125ae6c7564fcf7bac6ff5e5abdc3b525a"
	ci := map[string]string{"attrs": "ci", "bundle": "/nix/store/x-flake.bundle", "node": "leob", "sha": sha}
	web := map[string]string{"attrs": "keel-web-check keel-web-vm", "bundle": "/nix/store/x-flake.bundle", "node": "leob", "sha": sha}
	h.Live = append(h.Live, flakeCheck(4709, ci), flakeCheck(4710, web))
	if v := Render(h, nil, nil, nil); len(v.Dupes) != 0 {
		t.Fatalf("same sha, different attrs flagged: %v", v.Dupes)
	}
	h.Live = append(h.Live, flakeCheck(4711, ci))
	if v := Render(h, nil, nil, nil); !has(v.Dupes, "dupe flake.check 0d124712: 4709,4711") || len(v.Dupes) != 1 {
		t.Fatalf("identical params not a dupe: %v", v.Dupes)
	}
	// An older hub (no params_sum): the visible params are identical, the hidden ones unknown: never a dupe.
	for i := range h.Live {
		h.Live[i].Payload.ParamsSum = ""
	}
	if v := Render(h, nil, nil, nil); len(v.Dupes) != 0 {
		t.Fatalf("a row without the full params was grouped: %v", v.Dupes)
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
	if v := Render(h, nil, map[string]string{br: "1c93d0cbdb78231be750c9e188777b9b47124a9d"}, nil); len(v.Dupes) != 0 {
		t.Fatalf("tip == sha flagged: %v", v.Dupes)
	}
	const sha, tip = "1c93d0cbdb78231be750c9e188777b9b47124a9d", "ffffffffffffffffffffffffffffffffffffffff"
	moved := map[string]string{br: tip}
	// A different tip is flagged only when it is a PROVEN descendant of the job's sha.
	if v := Render(h, nil, moved, map[string]bool{Pair(sha, tip): true}); !has(v.Dupes, "superseded 4246 aether.clippy-pr") || !has(v.Dupes, "tip=ffffffff") {
		t.Fatalf("descendant tip not flagged: %v", v.Dupes)
	}
	// An older (or diverged) tip: the forge lags the node mirrors (jobs 4664-4669). Never flagged; nor with no verdict.
	if v := Render(h, nil, moved, map[string]bool{Pair(sha, tip): false}); len(v.Dupes) != 0 {
		t.Fatalf("older tip flagged: %v", v.Dupes)
	}
	if v := Render(h, nil, moved, nil); len(v.Dupes) != 0 {
		t.Fatalf("tip without a verdict flagged: %v", v.Dupes)
	}
	// absent upstream is unknown (mirror-only branches, e.g. jobs 4378-4401): never flagged.
	if v := Render(h, nil, map[string]string{br: ""}, nil); len(v.Dupes) != 0 {
		t.Fatalf("branch absent upstream flagged: %v", v.Dupes)
	}
	// main moving under a pinned eval is not supersession.
	if v := Render(h, nil, map[string]string{"main": "ffffffffffffffffffffffffffffffffffffffff"}, nil); has(v.Dupes, "superseded") {
		t.Fatalf("trunk flagged: %v", v.Dupes)
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
	q := QueueStats{Queue: "q"}
	q.Counts.Ready = 1
	h.Stats.Queues = append(h.Stats.Queues, q)
	out, _ := json.Marshal(Render(h, nil, nil, nil))
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("a log field reached the view: %s", out)
	}
	if !strings.Contains(string(out), "q ready=1 leased=0 oldest_ready=1@") {
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
	r := Step(ctx, State{}, h, Config{}, now, nil, nil)
	if strings.Join(r.Changed, ",") != "queues,nodes,dupes" || len(r.Set) != 4 || r.Set["updated"] == nil || strings.Join(r.Unset, ",") != "live" {
		t.Fatalf("first round: changed %v, %d keys, unset %v", r.Changed, len(r.Set), r.Unset)
	}
	t.Logf("first round: %d bytes", setBytes(r.Set))
	h.Stats.Hub.Now += 60_000
	r2 := Step(ctx, r.State, h, Config{}, now.Add(time.Minute), nil, nil)
	if len(r2.Set) != 0 || len(r2.Unset) != 0 {
		t.Fatalf("a quiet re-poll wrote %v, unset %v", r2.Changed, r2.Unset)
	}
	// A new ready job that is not the oldest: counts move (the hub's stats), nothing else.
	j := clone(readyJob(t, h), 9200, map[string]string{"sha": "0000000000000000000000000000000000000007"})
	j.Created = h.Stats.Hub.Now
	h.Live = append(h.Live, j)
	bump(h, j.Queue)
	r3 := Step(ctx, r2.State, h, Config{}, now.Add(2*time.Minute), nil, nil)
	if strings.Join(r3.Changed, ",") != "queues" || r3.Set["updated"] != nil || len(r3.Set) != 1 {
		t.Fatalf("a new job: changed %v (%d keys); want queues only (updated is a 10-min heartbeat)", r3.Changed, len(r3.Set))
	}
	// The warden's budget: a typical change (a job queued, a job claimed) is under 1 KB.
	if n := setBytes(r3.Set); n >= 1024 {
		t.Fatalf("a new job wrote %d bytes; budget 1 KB", n)
	}
	// A claim: ready -> leased, and the claiming node reports it running.
	h.Live = h.Live[:len(h.Live)-1]
	h.Nodes[0].Snapshot.Running = append(h.Nodes[0].Snapshot.Running, struct {
		HubJob int64 `json:"hub_job"`
	}{HubJob: 9200})
	for i := range h.Stats.Queues {
		if h.Stats.Queues[i].Queue == j.Queue {
			h.Stats.Queues[i].Counts.Ready--
			h.Stats.Queues[i].Counts.Leased++
		}
	}
	r4 := Step(ctx, r3.State, h, Config{}, now.Add(3*time.Minute), nil, nil)
	if strings.Join(r4.Changed, ",") != "queues,nodes" {
		t.Fatalf("a claim: changed %v; want queues,nodes", r4.Changed)
	}
	if n := setBytes(r4.Set); n >= 1024 {
		t.Fatalf("a claim wrote %d bytes; budget 1 KB", n)
	} else {
		t.Logf("a claim wrote %d bytes", n)
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
	desc := func(context.Context, string, string, string) (bool, error) { return true, nil }
	r := Step(ctx, State{}, h, Config{}, now, tip, desc)
	if calls != 1 || !has(r.State.View.Dupes, "superseded 4246") {
		t.Fatalf("first round: %d calls, dupes %v", calls, r.State.View.Dupes)
	}
	r = Step(ctx, r.State, h, Config{}, now.Add(4*time.Minute), tip, desc)
	if calls != 1 {
		t.Fatalf("tips asked again within %s: %d calls", TipEvery, calls)
	}
	fail = true
	r = Step(ctx, r.State, h, Config{}, now.Add(6*time.Minute), tip, desc)
	if calls != 2 || r.State.TipErr == "" || !has(r.State.View.Dupes, "superseded 4246") {
		t.Fatalf("a failed refresh must keep the old tips and say so: calls %d err %q dupes %v", calls, r.State.TipErr, r.State.View.Dupes)
	}
}

func readyJob(t *testing.T, h *Hub) Job {
	t.Helper()
	for _, j := range h.Live {
		if j.State == "ready" {
			return j
		}
	}
	t.Fatal("fixture has no ready job")
	return Job{}
}

func bump(h *Hub, queue string) {
	for i := range h.Stats.Queues {
		if h.Stats.Queues[i].Queue == queue {
			h.Stats.Queues[i].Counts.Ready++
		}
	}
}

func setBytes(set map[string]json.RawMessage) int {
	n := 0
	for k, v := range set {
		n += len(k) + len(v)
	}
	return n
}

// The descent check through Step: an OLDER tip (compare: behind) is never flagged, a true descendant is; each
// (sha, tip) pair costs one call, cached across rounds; an error is unknown (nothing flagged), not cached, and
// retried no sooner than TipEvery; a pair no live job needs leaves the cache.
func TestStepDescent(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	const older, newer = "0000000000000000000000000000000000000001", "0000000000000000000000000000000000000002"
	cur := older
	tip := func(context.Context, string, string) (string, error) { return cur, nil }
	calls := map[string]int{}
	fail := false
	desc := func(_ context.Context, repo, base, head string) (bool, error) {
		calls[base+"..."+head]++
		if fail {
			return false, fmt.Errorf("HTTP 502")
		}
		return head == newer, nil // the forge's compare: newer is ahead of the job's sha, older is behind it
	}
	r := Step(ctx, State{}, h, Config{}, now, tip, desc)
	if has(r.State.View.Dupes, "superseded") || len(calls) != 1 {
		t.Fatalf("older tip: dupes %v, calls %v", r.State.View.Dupes, calls)
	}
	r = Step(ctx, r.State, h, Config{}, now.Add(time.Minute), tip, desc)
	for p, n := range calls {
		if n != 1 {
			t.Fatalf("pair %s asked %d times; cached verdicts cost one call", p, n)
		}
	}
	// The forge catches up: the tip is now a descendant (asked at the next tip refresh).
	cur, fail = newer, true
	r = Step(ctx, r.State, h, Config{}, now.Add(6*time.Minute), tip, desc)
	if has(r.State.View.Dupes, "superseded") || r.State.DescErr == "" {
		t.Fatalf("a failed compare must be unknown and say so: dupes %v err %q", r.State.View.Dupes, r.State.DescErr)
	}
	before := len(calls)
	fail = false
	r = Step(ctx, r.State, h, Config{}, now.Add(7*time.Minute), tip, desc)
	if len(calls) != before || calls[Pair("1c93d0cbdb78231be750c9e188777b9b47124a9d", newer)] != 1 {
		t.Fatalf("retried inside the back-off: %v", calls)
	}
	r = Step(ctx, r.State, h, Config{}, now.Add(12*time.Minute), tip, desc)
	if !has(r.State.View.Dupes, "superseded 4246") || r.State.DescErr != "" {
		t.Fatalf("descendant tip not flagged after the back-off: dupes %v err %q", r.State.View.Dupes, r.State.DescErr)
	}
	if len(r.State.Desc) != 1 {
		t.Fatalf("cache keeps only pairs live jobs need: %v", r.State.Desc)
	}
}
