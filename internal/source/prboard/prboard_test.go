package prboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// fakePR is one upstream PR the fake GitHub serves.
type fakePR struct {
	n          int
	title      string
	state      string // OPEN, MERGED, CLOSED
	draft      bool
	head       string
	updated    time.Time
	mergeable  string
	checks     []map[string]any // contexts
	comments   []map[string]any
	mergedAt   string
	mergeSha   string
	unresolved int
}

func (p *fakePR) gql() map[string]any {
	ctxs := []any{}
	for _, c := range p.checks {
		ctxs = append(ctxs, c)
	}
	threads := []any{}
	for i := 0; i < p.unresolved; i++ {
		threads = append(threads, map[string]any{"isResolved": false})
	}
	comments := []any{}
	for _, c := range p.comments {
		comments = append(comments, c)
	}
	m := map[string]any{"number": p.n, "title": p.title, "url": fmt.Sprintf("https://x/pull/%d", p.n), "state": p.state,
		"isDraft": p.draft, "mergeable": p.mergeable, "mergeStateStatus": "BLOCKED", "headRefOid": p.head, "headRefName": "b",
		"baseRefName": "main", "additions": 10, "deletions": 2, "createdAt": "2026-11-09T00:00:00Z",
		"updatedAt": p.updated.Format(time.RFC3339), "mergedAt": p.mergedAt, "closedAt": p.mergedAt,
		"reviewThreads": map[string]any{"nodes": threads}, "comments": map[string]any{"nodes": comments},
		"latestOpinionatedReviews": map[string]any{"nodes": []any{}},
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{
			"state": "SUCCESS", "contexts": map[string]any{"nodes": ctxs}}}}}}}
	if p.mergeSha != "" {
		m["mergeCommit"] = map[string]any{"oid": p.mergeSha}
	}
	return m
}

// fakeGH answers searchQuery and batchQuery from its PR list, and counts calls.
type fakeGH struct {
	t       *testing.T
	prs     map[int]*fakePR
	calls   int
	queries []string
}

var (
	cursorRE = regexp.MustCompile(`updated:>=(\S+?)\\?"`)
	aliasRE  = regexp.MustCompile(`p(\d+): pullRequest`)
)

func (g *fakeGH) GraphQL(_ context.Context, q string) (json.RawMessage, error) {
	g.calls++
	g.queries = append(g.queries, q)
	if strings.HasPrefix(q, "{search(") {
		var nodes []any
		var since time.Time
		if m := cursorRE.FindStringSubmatch(q); m != nil {
			var err error
			if since, err = time.Parse(time.RFC3339, m[1]); err != nil {
				g.t.Fatalf("cursor %q: %v", m[1], err)
			}
		}
		open := strings.Contains(q, "is:open")
		for _, p := range g.prs {
			if open && p.state != "OPEN" || !open && p.updated.Before(since) {
				continue
			}
			nodes = append(nodes, p.gql())
		}
		return model.MustJSON(map[string]any{"search": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}), nil
	}
	repo := map[string]any{}
	for _, m := range aliasRE.FindAllStringSubmatch(q, -1) {
		n, _ := strconv.Atoi(m[1])
		if p, ok := g.prs[n]; ok {
			repo["p"+m[1]] = p.gql()
		}
	}
	return model.MustJSON(map[string]any{"repository": repo}), nil
}

type fakeHub struct {
	jobs  []HubJob
	tests map[int64]int
}

func (h *fakeHub) Jobs(context.Context, time.Time) ([]HubJob, error) { return h.jobs, nil }
func (h *fakeHub) Tests(_ context.Context, id int64) (int, bool, error) {
	n, ok := h.tests[id]
	return n, ok, nil
}

func hubJob(id int64, queue, sha, state string, ok bool) HubJob {
	j := HubJob{ID: id, Queue: queue, State: state}
	j.Payload.Params = map[string]json.RawMessage{"sha": model.MustJSON(sha)}
	if state == "done" {
		j.Result.OK = &ok
	}
	return j
}

func policyText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../mergecheck/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Rule string `json:"rule"`
	}
	_ = json.Unmarshal(b, &g)
	if len(g.Rule) < 500 {
		t.Fatalf("policy text %d B", len(g.Rule))
	}
	return g.Rule
}

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type world struct {
	t     *testing.T
	gh    *fakeGH
	hub   *fakeHub
	st    State
	now   time.Time
	board map[string]json.RawMessage
	in    map[string]json.RawMessage
}

func newWorld(t *testing.T) *world {
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	w := &world{t: t, now: now, hub: &fakeHub{tests: map[int64]int{}},
		in: map[string]json.RawMessage{"rule": model.MustJSON(policyText(t))}}
	w.gh = &fakeGH{t: t, prs: map[int]*fakePR{
		101: {n: 101, title: "[CACHE][1-2] first", state: "OPEN", head: headA, mergeable: "MERGEABLE", updated: now.Add(-time.Hour),
			checks: []map[string]any{{"name": "Aether Runtime PR Pipeline", "conclusion": "SUCCESS"}}},
		102: {n: 102, title: "[CACHE][2-2] second", state: "OPEN", draft: true, head: headB, mergeable: "MERGEABLE", updated: now.Add(-time.Hour)},
	}}
	return w
}

func (w *world) pass(dt time.Duration) Result {
	w.t.Helper()
	w.now = w.now.Add(dt)
	w.gh.calls = 0
	r, err := Run(context.Background(), w.gh, w.hub, w.st, Pass{Watch: model.BoardWatch{Author: "John-Kelliher", Repo: "bic/aether"},
		Inputs: w.in, Board: w.board, Now: w.now})
	if err != nil {
		w.t.Fatal(err)
	}
	w.st = r.State
	return r
}

func childWrites(r Result) map[string]plugin.FormWrite {
	out := map[string]plugin.FormWrite{}
	for _, f := range r.Forms {
		if f.Key != "" {
			out[f.Key] = f
		}
	}
	return out
}

// The cost rule: a pass over our PRs when nothing changed is ONE GitHub call (the search) and writes nothing.
func TestQuietPassCostsOneCall(t *testing.T) {
	w := newWorld(t)
	r := w.pass(0)
	if len(childWrites(r)) != 2 || !strings.Contains(w.gh.queries[0], "is:open") {
		t.Fatalf("first pass: %d forms, query %.200s", len(childWrites(r)), w.gh.queries[0])
	}
	for i := 0; i < 3; i++ {
		r = w.pass(2 * time.Minute)
		if r.GHCalls != 1 || w.gh.calls != 1 || len(r.Forms) != 0 {
			t.Fatalf("quiet pass %d: %d GitHub calls, %d writes", i, r.GHCalls, len(r.Forms))
		}
	}
	if q := w.gh.queries[len(w.gh.queries)-1]; !strings.Contains(q, "updated:>="+w.now.Add(-2*time.Minute-CursorSkew).Format(time.RFC3339)) {
		t.Fatalf("cursor not used: %.300s", q)
	}
}

// A push (updatedAt moves) is found by the search and rewrites that PR's form only.
func TestUpdateWritesOnlyThatForm(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.prs[101]
	p.head, p.updated = "cccccccccccccccccccccccccccccccccccccccc", w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	cw := childWrites(r)
	if len(cw) != 1 || cw["pr-101"].Set == nil || !strings.Contains(string(cw["pr-101"].Set["head"]), "cccc") {
		t.Fatalf("writes %v", cw)
	}
	for k := range cw["pr-101"].Set {
		if k == "note" || strings.HasPrefix(k, "note") {
			t.Fatalf("a fact write carries %s", k)
		}
	}
}

// Checks still running at a pass are re-read next pass even though updatedAt does not move (CI completion), in one
// batch call; once settled they cost nothing again.
func TestSettlingChecksAreReRead(t *testing.T) {
	w := newWorld(t)
	w.gh.prs[101].checks = []map[string]any{{"name": "Aether Runtime PR Pipeline", "status": "IN_PROGRESS"}}
	w.pass(0)
	// Still running: re-read (a second call), nothing moved, nothing written.
	if r := w.pass(2 * time.Minute); r.GHCalls != 2 || len(r.Forms) != 0 {
		t.Fatalf("unchanged re-read: %d calls, %d writes", r.GHCalls, len(r.Forms))
	}
	w.gh.prs[101].checks = []map[string]any{{"name": "Aether Runtime PR Pipeline", "conclusion": "SUCCESS"}}
	r := w.pass(2 * time.Minute)
	if r.GHCalls != 2 || !strings.Contains(string(childWrites(r)["pr-101"].Set["checks"]), "SUCCESS") {
		t.Fatalf("re-read: %d calls, writes %v", r.GHCalls, childWrites(r))
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("settled PR still re-read: %d calls", r.GHCalls)
	}
}

// A band job finishing on a head changes the checklist: the PR is re-read and its form rewritten. Its test count
// comes from the hub; an unknown count never passes.
func TestBandJobsAndTestCounts(t *testing.T) {
	w := newWorld(t)
	w.hub.jobs = []HubJob{hubJob(7, "aether.gate-kit", headA, "leased", false)}
	r := w.pass(0)
	if !strings.Contains(string(childWrites(r)["pr-101"].Set["pstate"]), "waiting: band jobs") {
		t.Fatalf("pstate %s", childWrites(r)["pr-101"].Set["pstate"])
	}
	w.hub.jobs = []HubJob{hubJob(7, "aether.gate-kit", headA, "done", true)}
	r = w.pass(2 * time.Minute)
	cl := string(childWrites(r)["pr-101"].Set["checklist"])
	if r.GHCalls != 2 || !strings.Contains(cl, "tests not read yet") || !strings.Contains(cl, `"status":"pending"`) {
		t.Fatalf("PASS with an unknown count: calls %d checklist %.400s", r.GHCalls, cl)
	}
	w.hub.tests[7] = 412
	r = w.pass(2 * time.Minute)
	if cl := string(childWrites(r)["pr-101"].Set["checklist"]); !strings.Contains(cl, "412 tests") {
		t.Fatalf("known count: %.400s", cl)
	}
	if w.st.Tests[7] != 412 {
		t.Fatalf("count not cached: %v", w.st.Tests)
	}
}

// The architect verdict is read from the bot's comment in the same query.
func TestArchitectVerdictFromComments(t *testing.T) {
	w := newWorld(t)
	w.gh.prs[101].comments = []map[string]any{
		{"author": map[string]any{"login": "architect-review"}, "createdAt": "2026-11-10T20:00:00Z",
			"body": "<!-- architect-review -->\nArchitect review \u2014 PR #101 @ `aaaaaaaa`\n\n**APPROVE**\n"},
		{"author": map[string]any{"login": "someone"}, "createdAt": "2026-11-10T21:00:00Z", "body": "<!-- architect-review -->"},
	}
	r := w.pass(0)
	a := string(childWrites(r)["pr-101"].Set["architect"])
	if !strings.Contains(a, `"verdict":"APPROVE"`) || !strings.Contains(a, `"sha":"aaaaaaaa"`) {
		t.Fatalf("architect %s", a)
	}
}

// Merge: the final facts are written and the ledger record goes to the inputs form's outbox; the form retires only
// after an ack newer than that write, which also clears the outbox entry and stops tracking.
func TestMergeLedgerRetire(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.prs[101]
	p.state, p.mergedAt, p.mergeSha, p.updated = "MERGED", w.now.Add(time.Minute).Format(time.RFC3339), "dddd", w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	var outbox map[string]json.RawMessage
	for _, f := range r.Forms {
		if f.Inputs {
			_ = json.Unmarshal(f.Set["ledger_outbox"], &outbox)
		}
	}
	rec := string(outbox["101"])
	if !strings.Contains(rec, `"kind":"pr-final"`) || !strings.Contains(rec, `"merge_sha":"dddd"`) || !strings.Contains(rec, `"hash"`) {
		t.Fatalf("record %s", rec)
	}
	if !strings.Contains(string(childWrites(r)["pr-101"].Set["state"]), "MERGED") {
		t.Fatal("final facts not written")
	}
	written := w.st.PRs[101].Outbox
	w.in["ledger_outbox"] = model.MustJSON(outbox)
	w.in["ledger_ack"] = model.MustJSON(map[string]any{"101": map[string]string{"sha": "k1", "path": "p", "at": "2026-11-11T00:00:00Z", "hash": "x"}})
	if r := w.pass(2 * time.Minute); childWrites(r)["pr-101"].Retire {
		t.Fatal("retired on an ack older than the outbox write")
	}
	w.in["ledger_ack"] = model.MustJSON(map[string]any{"101": map[string]string{"sha": "k1", "path": "p", "at": w.now.Add(time.Minute).Format(time.RFC3339)}})
	r = w.pass(2 * time.Minute)
	if !childWrites(r)["pr-101"].Retire || w.st.PRs[101] != nil || written == "" {
		t.Fatalf("not retired after the ack: %v tracked %v", childWrites(r), w.st.PRs[101])
	}
	cleared := false
	for _, f := range r.Forms {
		if f.Inputs && strings.Contains(string(f.Set["ledger_outbox"]), `"101":null`) {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("outbox entry not cleared")
	}
}

// B: the board is reconciled from tracked PRs (title tags) and written only when it changes.
func TestBoardWrittenOnlyWhenItChanges(t *testing.T) {
	w := newWorld(t)
	w.board = map[string]json.RawMessage{"prs": model.MustJSON(map[string]any{}), "stacks": model.MustJSON(map[string]any{"cache": map[string]any{}})}
	r := w.pass(0)
	var set json.RawMessage
	for _, f := range r.Forms {
		if f.Board && f.Set["prs"] != nil {
			set = f.Set["prs"]
		}
	}
	if !strings.Contains(string(set), `"pr":101`) || !strings.Contains(string(set), `"pr":102`) {
		t.Fatalf("board prs %s", set)
	}
	w.board["prs"] = set
	for _, f := range w.pass(2 * time.Minute).Forms {
		if f.Board && f.Set["prs"] != nil {
			t.Fatalf("an unchanged board was rewritten: %s", f.Set["prs"])
		}
	}
}
