package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// item is one upstream PR or issue the fake GitHub serves.
type item struct {
	n          int
	issue      bool
	author     string
	title      string
	state      string // OPEN, MERGED, CLOSED
	draft      bool
	head       string
	updated    time.Time
	mergeable  string
	checks     []map[string]any // contexts
	comments   []map[string]any
	reviews    []map[string]any
	labels     []string
	files      []string
	body       string
	mergedAt   string
	closedAt   string
	unresolved int
}

func (p *item) gql() map[string]any {
	labels := []any{}
	for _, l := range p.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	base := map[string]any{"number": p.n, "title": p.title, "url": fmt.Sprintf("https://x/%d", p.n), "state": p.state,
		"updatedAt": p.updated.Format(time.RFC3339), "closedAt": p.closedAt, "author": map[string]any{"login": p.author},
		"labels": map[string]any{"nodes": labels}}
	if p.issue {
		base["__typename"] = "Issue"
		return base
	}
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
	reviews := []any{}
	for _, r := range p.reviews {
		reviews = append(reviews, r)
	}
	files, more := filePage(p.files, 0)
	for k, v := range map[string]any{"__typename": "PullRequest", "isDraft": p.draft, "mergeable": p.mergeable,
		"mergeStateStatus": "BLOCKED", "headRefOid": p.head, "baseRefName": "main", "mergedAt": p.mergedAt, "body": p.body,
		"files":         map[string]any{"totalCount": len(p.files), "pageInfo": map[string]any{"hasNextPage": more != "", "endCursor": more}, "nodes": files},
		"reviewThreads": map[string]any{"nodes": threads}, "comments": map[string]any{"nodes": comments},
		"latestOpinionatedReviews": map[string]any{"nodes": reviews},
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{
			"state": "SUCCESS", "contexts": map[string]any{"nodes": ctxs}}}}}}} {
		base[k] = v
	}
	return base
}

// filePage is the page of files from offset: 100 a page, the cursor the next offset.
func filePage(files []string, from int) ([]any, string) {
	out := []any{}
	end := min(from+100, len(files))
	for _, f := range files[from:end] {
		out = append(out, map[string]any{"path": f})
	}
	if end < len(files) {
		return out, strconv.Itoa(end)
	}
	return out, ""
}

// fakeGH answers searchQuery, batchQuery and filesQuery from its items, counts calls, and pages searches pageSize at a
// time (0: one page).
type fakeGH struct {
	t        *testing.T
	items    map[int]*item
	calls    int
	queries  []string
	pageSize int
	fail     error
}

var (
	searchRE = regexp.MustCompile(`a(\d+): search\(type:ISSUE, first:\d+(?:, after:"([^"]*)")?, query:"([^"]*)"\)`)
	cursorRE = regexp.MustCompile(`updated:>=(\S+)`)
	authorRE = regexp.MustCompile(`author:(\S+)`)
	prRE     = regexp.MustCompile(`p(\d+): pullRequest`)
	issueRE  = regexp.MustCompile(`i(\d+): issueOrPullRequest`)
	filesRE  = regexp.MustCompile(`pullRequest\(number:(\d+)\)\{files\(first:100, after:"([^"]*)"\)`)
)

func (g *fakeGH) GraphQL(_ context.Context, q string) (json.RawMessage, error) {
	g.calls++
	g.queries = append(g.queries, q)
	if g.fail != nil {
		return nil, g.fail
	}
	if strings.HasPrefix(q, "{a") {
		out := map[string]any{}
		for _, m := range searchRE.FindAllStringSubmatch(q, -1) {
			query := m[3]
			var since time.Time
			if c := cursorRE.FindStringSubmatch(query); c != nil {
				var err error
				if since, err = time.Parse(time.RFC3339, c[1]); err != nil {
					g.t.Fatalf("cursor %q: %v", c[1], err)
				}
			}
			author := authorRE.FindStringSubmatch(query)[1]
			open := strings.Contains(query, "is:open")
			var match []*item
			for _, it := range g.items {
				if it.author != author || open && it.state != "OPEN" || !open && it.updated.Before(since) {
					continue
				}
				match = append(match, it)
			}
			sort.Slice(match, func(i, j int) bool { return match[i].n < match[j].n })
			from, _ := strconv.Atoi(m[2])
			end := len(match)
			if g.pageSize > 0 {
				end = min(from+g.pageSize, len(match))
			}
			nodes := []any{}
			for _, it := range match[from:end] {
				nodes = append(nodes, it.gql())
			}
			out["a"+m[1]] = map[string]any{"pageInfo": map[string]any{"hasNextPage": end < len(match), "endCursor": strconv.Itoa(end)}, "nodes": nodes}
		}
		return model.MustJSON(out), nil
	}
	if m := filesRE.FindStringSubmatch(q); m != nil {
		n, _ := strconv.Atoi(m[1])
		from, _ := strconv.Atoi(m[2])
		files, more := filePage(g.items[n].files, from)
		return model.MustJSON(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"files": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": more != "", "endCursor": more}, "nodes": files}}}}), nil
	}
	repo := map[string]any{}
	for _, m := range prRE.FindAllStringSubmatch(q, -1) {
		n, _ := strconv.Atoi(m[1])
		if it, ok := g.items[n]; ok && !it.issue {
			repo["p"+m[1]] = it.gql()
		}
	}
	for _, m := range issueRE.FindAllStringSubmatch(q, -1) {
		n, _ := strconv.Atoi(m[1])
		it, ok := g.items[n]
		if !ok {
			return nil, fmt.Errorf("graphql: Could not resolve to an issue or pull request with the number of %d", n)
		}
		if it.issue {
			repo["i"+m[1]] = it.gql()
		} else {
			repo["i"+m[1]] = map[string]any{"__typename": "PullRequest"}
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
	headC = "cccccccccccccccccccccccccccccccccccccccc"
	ours  = "John-Kelliher"
	his   = "Chiqun-Zhang"
)

type world struct {
	t     *testing.T
	gh    *fakeGH
	hub   *fakeHub
	st    State
	now   time.Time
	board map[string]json.RawMessage
	in    map[string]json.RawMessage
	watch model.ScoutWatch
}

// newWorld: 101 ours (title tag), 102 ours draft (label), 201 Chiqun's (path hit), 103 ours unrelated (no hit), 301 an
// issue of ours with the label, 302 an unrelated issue of ours.
func newWorld(t *testing.T) *world {
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	w := &world{t: t, now: now, hub: &fakeHub{tests: map[int64]int{}},
		in: map[string]json.RawMessage{"rule": model.MustJSON(policyText(t))},
		watch: model.ScoutWatch{Repo: "bic/aether", Authors: []string{ours, his}, Labels: []string{"effort-router"},
			TitleMatch: `^\[(Effort|MD1)[^\]]*\]`, Paths: []string{"**/decision_judge/**", "**/system1*"}}}
	old := now.Add(-time.Hour)
	w.gh = &fakeGH{t: t, items: map[int]*item{
		101: {n: 101, author: ours, title: "[Effort router][1-2] first", state: "OPEN", head: headA, mergeable: "MERGEABLE", updated: old,
			checks: []map[string]any{{"name": "Aether Runtime PR Pipeline", "conclusion": "SUCCESS"}}},
		102: {n: 102, author: ours, title: "second", labels: []string{"Effort-Router"}, state: "OPEN", draft: true, head: headB, mergeable: "MERGEABLE", updated: old},
		201: {n: 201, author: his, title: "refactor(x): y", state: "OPEN", head: headC, mergeable: "MERGEABLE", updated: old,
			files: []string{"README.md", "aether_rust/crates/s/src/decision_judge/mod.rs"}},
		103: {n: 103, author: ours, title: "docs: unrelated", state: "OPEN", head: headB, mergeable: "MERGEABLE", updated: old, files: []string{"docs/a.md"}},
		301: {n: 301, issue: true, author: ours, title: "plan item", labels: []string{"effort-router"}, state: "OPEN", updated: old},
		302: {n: 302, issue: true, author: ours, title: "unrelated issue", state: "OPEN", updated: old},
	}}
	return w
}

func (w *world) pass(dt time.Duration) Result {
	w.t.Helper()
	r, err := w.try(dt)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) try(dt time.Duration) (Result, error) {
	w.now = w.now.Add(dt)
	w.gh.calls = 0
	r, err := Run(context.Background(), w.gh, w.hub, w.st, Pass{Watch: w.watch, Inputs: w.in, Board: w.board, Now: w.now})
	if err == nil {
		w.st = r.State
	}
	return r, err
}

func setKeys(r Result) []string {
	var out []string
	for k := range r.Set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func entry(t *testing.T, r Result, key string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Set[key], &m); err != nil {
		t.Fatalf("%s: %v (%s)", key, err, r.Set[key])
	}
	return m
}

func outboxOf(r Result) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, f := range r.Forms {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(f.Set["ledger_outbox"], &m)
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// S1, the cost rule: one search per author, all in ONE GraphQL call; a pass when nothing changed is that call and
// nothing else, and writes nothing.
func TestQuietPassCostsOneCall(t *testing.T) {
	w := newWorld(t)
	r := w.pass(0)
	q := w.gh.queries[0]
	if r.GHCalls != 1 || !strings.Contains(q, "author:"+ours) || !strings.Contains(q, "author:"+his) || !strings.Contains(q, "is:open") {
		t.Fatalf("first pass: %d calls, query %.300s", r.GHCalls, q)
	}
	if got := strings.Join(setKeys(r), " "); got != "issue:301 pr:101 pr:102 pr:201" {
		t.Fatalf("first pass entries: %s", got)
	}
	for i := 0; i < 3; i++ {
		r = w.pass(2 * time.Minute)
		if r.GHCalls != 1 || w.gh.calls != 1 || len(r.Set)+len(r.Unset)+len(r.Forms) != 0 {
			t.Fatalf("quiet pass %d: %d GitHub calls, set %v unset %v forms %d", i, r.GHCalls, setKeys(r), r.Unset, len(r.Forms))
		}
	}
	if q := w.gh.queries[len(w.gh.queries)-1]; !strings.Contains(q, "updated:>="+w.now.Add(-2*time.Minute-CursorSkew).Format(time.RFC3339)) {
		t.Fatalf("cursor not used: %.300s", q)
	}
}

// Paging: an alias still paging gets its next page in the next call; a finished alias is not asked again.
func TestSearchPagesPerAlias(t *testing.T) {
	w := newWorld(t)
	w.gh.pageSize = 2
	r := w.pass(0)
	if got := strings.Join(setKeys(r), " "); got != "issue:301 pr:101 pr:102 pr:201" {
		t.Fatalf("paged entries: %s (%d calls)", got, r.GHCalls)
	}
	// ours: 101 102 103 301 302 (5, 3 pages); Chiqun's: 201 (1 page). Calls 2 and 3 name only a0.
	if r.GHCalls != 3 || strings.Contains(w.gh.queries[1], "a1:") || !strings.Contains(w.gh.queries[2], `after:"4"`) {
		t.Fatalf("%d calls: %q", r.GHCalls, w.gh.queries)
	}
}

// The entry is exactly the board-sync v2 field list, nothing else (no bodies).
func TestEntryFieldsExactlyTheList(t *testing.T) {
	w := newWorld(t)
	p := w.gh.items[101]
	p.comments = []map[string]any{
		{"author": map[string]any{"login": "architect-review"}, "createdAt": "2026-11-10T20:00:00Z",
			"body": "<!-- architect-review -->\nArchitect review \u2014 PR #101 @ `aaaaaaaa`\n\n**APPROVE**\n"},
		{"author": map[string]any{"login": "bob"}, "createdAt": "2026-11-10T21:00:00Z", "body": "<!-- automaton:101:aaaaaaaa verdict:request_changes -->\n## Automaton Review"},
	}
	p.reviews = []map[string]any{{"author": map[string]any{"login": "lukas"}, "state": "APPROVED", "commit": map[string]any{"oid": headA}}}
	p.body = "a long body that must never reach the form " + strings.Repeat("x", 3000)
	p.unresolved = 2
	r := w.pass(0)
	e := entry(t, r, "pr:101")
	want := "architect author automaton base checks draft head8 kind lukas merge_state mergeable n state threads title updated_at"
	var got []string
	for k := range e {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != want {
		t.Fatalf("pr entry keys\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if e["kind"] != "pr" || e["author"] != ours || e["head8"] != "aaaaaaaa" || e["threads"] != 2.0 || e["mergeable"] != "MERGEABLE" {
		t.Fatalf("pr entry %v", e)
	}
	if len(r.Set["pr:101"]) > 1500 {
		t.Fatalf("entry is not slim: %d B", len(r.Set["pr:101"]))
	}
	is := entry(t, r, "issue:301")
	got = nil
	for k := range is {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "author kind labels n state title updated_at" || is["kind"] != "issue" {
		t.Fatalf("issue entry %v", is)
	}
	// merged_at appears once merged.
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	if e := entry(t, w.pass(2*time.Minute), "pr:101"); e["merged_at"] != p.mergedAt || e["merge_state"] != "merged" {
		t.Fatalf("merged entry %v", e)
	}
}

// Verdicts: the architect's (and a newer hold), lukas's review, the Automaton's marker (its queue ack is not a
// verdict), each with the head it read.
func TestVerdicts(t *testing.T) {
	w := newWorld(t)
	p := w.gh.items[101]
	p.comments = []map[string]any{
		{"author": map[string]any{"login": "architect-review"}, "createdAt": "2026-11-10T20:00:00Z",
			"body": "<!-- architect-review -->\nArchitect review \u2014 PR #101 @ `aaaaaaaa`\n\n**APPROVE_WITH_NOTES**\n"},
		{"author": map[string]any{"login": "someone"}, "createdAt": "2026-11-10T21:00:00Z", "body": "<!-- architect-review -->"},
		{"author": map[string]any{"login": "bob"}, "createdAt": "2026-11-10T19:00:00Z", "body": "<!-- automaton:101:bbbbbbbb verdict:approve -->"},
		{"author": map[string]any{"login": "bob"}, "createdAt": "2026-11-10T22:00:00Z", "body": "<!-- automaton-ack:101:aaaaaaaa -->"},
		{"author": map[string]any{"login": "bob"}, "createdAt": "2026-11-10T22:30:00Z", "body": "<!-- automaton:999:aaaaaaaa verdict:approve -->"},
	}
	p.reviews = []map[string]any{{"author": map[string]any{"login": "lukas"}, "state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": headB}}}
	e := entry(t, w.pass(0), "pr:101")
	if s := fmt.Sprint(e["architect"]); s != "map[at:2026-11-10T20:00:00Z head:aaaaaaaa verdict:APPROVE_WITH_NOTES]" {
		t.Fatalf("architect %s", s)
	}
	if s := fmt.Sprint(e["automaton"]); s != "map[at:2026-11-10T19:00:00Z head:bbbbbbbb verdict:APPROVE]" {
		t.Fatalf("automaton %s", s)
	}
	if s := fmt.Sprint(e["lukas"]); s != "map[head:bbbbbbbb verdict:CHANGES_REQUESTED]" {
		t.Fatalf("lukas %s", s)
	}
	p.comments = append(p.comments, map[string]any{"author": map[string]any{"login": "architect-review[bot]"}, "createdAt": "2026-11-10T23:00:00Z",
		"body": "<!-- architect-review-hold -->\npass in progress"})
	p.updated = w.now.Add(time.Minute)
	if s := fmt.Sprint(entry(t, w.pass(2*time.Minute), "pr:101")["architect"]); s != "map[at:2026-11-10T23:00:00Z head:aaaaaaaa verdict:hold]" {
		t.Fatalf("held architect %s", s)
	}
}

// Write only on change: a push to one PR writes that entry alone.
func TestUpdateWritesOnlyThatEntry(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.head, p.updated = headC, w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	if got := strings.Join(setKeys(r), " "); got != "pr:101" || entry(t, r, "pr:101")["head8"] != "cccccccc" {
		t.Fatalf("writes %s", got)
	}
	// An update that changes nothing in the entry but updated_at still writes (updated_at is a field), and an update
	// to an untracked item writes nothing.
	w.gh.items[302].updated = w.now.Add(time.Minute)
	w.gh.items[103].updated = w.now.Add(time.Minute)
	if r := w.pass(2 * time.Minute); len(r.Set) != 0 {
		t.Fatalf("untracked updates wrote %v", setKeys(r))
	}
}

// The filter: label (any case), title, or a changed file. A path verdict is made once per head: a miss on a big PR
// (files beyond the first page) costs its file pages once, not every time the PR is updated; a new head is listed
// again; a hit on page 2 tracks it.
func TestFilterPathsOncePerHead(t *testing.T) {
	w := newWorld(t)
	big := make([]string, 250)
	for i := range big {
		big[i] = fmt.Sprintf("docs/f%03d.md", i)
	}
	w.gh.items[202] = &item{n: 202, author: his, title: "chore: many files", state: "OPEN", head: headA, mergeable: "MERGEABLE",
		updated: w.now.Add(-time.Hour), files: big}
	r := w.pass(0)
	if r.GHCalls != 3 || r.Set["pr:202"] != nil || r.Set["pr:103"] != nil || w.st.Skip[202] != headA || w.st.Skip[103] != headB {
		t.Fatalf("big miss: %d calls, set %v, skip %v", r.GHCalls, setKeys(r), w.st.Skip)
	}
	w.gh.items[202].updated = w.now.Add(time.Minute) // a comment: same head
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("a missed head was listed again: %d calls", r.GHCalls)
	}
	big[240] = "aether_rust/x/system1/plan.rs"
	w.gh.items[202].head, w.gh.items[202].updated = headC, w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	if r.GHCalls != 3 || r.Set["pr:202"] == nil || w.st.Skip[202] != "" {
		t.Fatalf("new head with a hit on page 3: %d calls, set %v", r.GHCalls, setKeys(r))
	}
}

// Issues: a label or title hit is tracked; an issue a tracked PR's body links is tracked too, read by number when
// the search did not return it (one non-fatal batch); a linked number that is a PR (or unreadable) is not asked again
// for MissingRetry.
func TestIssuesLinkedFromPRs(t *testing.T) {
	w := newWorld(t)
	w.gh.items[302].updated = w.now.Add(-48 * time.Hour)
	w.pass(0)
	// 101's body now links 302 (not updated since), PR 103 (untracked: a PR, not an issue) and 9999 (nothing).
	p := w.gh.items[101]
	p.body, p.updated = "Part of https://x/bic/aether/issues/302; needs #1030 and see #9999 and step #1.", w.now.Add(time.Minute)
	w.gh.items[1030] = &item{n: 1030, author: "someone", title: "a pr", state: "OPEN", head: headB, updated: w.now.Add(-time.Hour)}
	r := w.pass(2 * time.Minute)
	// call 1 the search, call 2 the linked batch (fails: 9999 does not exist), so nothing linked is tracked yet.
	if r.GHCalls != 2 || r.Set["issue:302"] != nil || !strings.Contains(strings.Join(r.Log, "\n"), "linked issues") {
		t.Fatalf("pass with an unreadable link: %d calls, set %v, log %v", r.GHCalls, setKeys(r), r.Log)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("a failed link was retried before MissingRetry: %d calls", r.GHCalls)
	}
	p.body, p.updated = "Part of https://x/bic/aether/issues/302; needs #1030.", w.now.Add(time.Minute)
	r = w.pass(MissingRetry)
	if r.GHCalls != 2 || r.Set["issue:302"] == nil || w.st.Missing[1030] == "" || w.st.Missing[302] != "" {
		t.Fatalf("linked issue: %d calls, set %v, missing %v", r.GHCalls, setKeys(r), w.st.Missing)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("linked numbers asked again: %d calls", r.GHCalls)
	}
	// Once tracked, an issue's own update is seen by the search.
	w.gh.items[302].state, w.gh.items[302].updated = "CLOSED", w.now.Add(time.Minute)
	if e := entry(t, w.pass(2*time.Minute), "issue:302"); e["state"] != "CLOSED" {
		t.Fatalf("issue 302 %v", e)
	}
}

// Checks still running at a pass are re-read next pass even though updatedAt does not move (CI completion), in one
// batch call; once settled they cost nothing again.
func TestSettlingChecksAreReRead(t *testing.T) {
	w := newWorld(t)
	w.gh.items[101].checks = []map[string]any{{"name": "Aether Runtime PR Pipeline", "status": "IN_PROGRESS"}}
	w.pass(0)
	if r := w.pass(2 * time.Minute); r.GHCalls != 2 || len(r.Set) != 0 {
		t.Fatalf("unchanged re-read: %d calls, %v", r.GHCalls, setKeys(r))
	}
	w.gh.items[101].checks = []map[string]any{{"name": "Aether Runtime PR Pipeline", "conclusion": "SUCCESS"}}
	r := w.pass(2 * time.Minute)
	if c := fmt.Sprint(entry(t, r, "pr:101")["checks"]); r.GHCalls != 2 || c != "map[fail:0 pass:1 pending:0 rollup:SUCCESS]" {
		t.Fatalf("re-read: %d calls, checks %s", r.GHCalls, c)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("settled PR still re-read: %d calls", r.GHCalls)
	}
}

// merge_state is the first unmet step of the rule. A band job finishing on a head moves it (re-read, rewritten);
// an unknown test count never passes the band step; a known one does.
func TestMergeStateAndTestCounts(t *testing.T) {
	w := newWorld(t)
	w.hub.jobs = []HubJob{hubJob(7, "aether.gate-kit", headA, "leased", false)}
	r0 := w.pass(0)
	if ms := entry(t, r0, "pr:101")["merge_state"]; ms != "waiting: band jobs" {
		t.Fatalf("merge_state %v", ms)
	}
	if ms := entry(t, r0, "pr:102")["merge_state"]; ms != "draft" {
		t.Fatalf("draft merge_state %v", ms)
	}
	w.hub.jobs = []HubJob{hubJob(7, "aether.gate-kit", headA, "done", true)}
	r := w.pass(2 * time.Minute)
	if r.GHCalls != 2 || r.Set["pr:101"] != nil {
		t.Fatalf("PASS with an unknown count moved the entry: %d calls, %s", r.GHCalls, r.Set["pr:101"])
	}
	w.hub.tests[7] = 412
	r = w.pass(2 * time.Minute)
	if ms := entry(t, r, "pr:101")["merge_state"]; ms == "waiting: band jobs" || !strings.HasPrefix(fmt.Sprint(ms), "waiting: ") {
		t.Fatalf("known count: merge_state %v", ms)
	}
	if w.st.Tests[7] != 412 {
		t.Fatalf("count not cached: %v", w.st.Tests)
	}
	w.hub.jobs = nil // the job left the hub's window: its count is forgotten
	w.pass(2 * time.Minute)
	if _, kept := w.st.Tests[7]; kept {
		t.Fatalf("a count outlived its job: %v", w.st.Tests)
	}
	// No rule on the inputs form: merge_state unknown, said in the log.
	w.in = map[string]json.RawMessage{}
	w.gh.items[101].updated = w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	if ms := entry(t, r, "pr:101")["merge_state"]; ms != "unknown" || !strings.Contains(strings.Join(r.Log, " "), "no merge policy") {
		t.Fatalf("no rule: %v %v", ms, r.Log)
	}
}

// The tests route: {"tests_run": n} is a count; 404, null, or a missing key is unknown.
func TestTestsRun(t *testing.T) {
	for _, c := range []struct {
		body string
		n    int
		ok   bool
	}{{`{"id":7,"state":"done","tests_run":1234}`, 1234, true}, {`{"id":7,"tests_run":0}`, 0, true},
		{`{"id":7,"tests_run":null}`, 0, false}, {`{"id":7}`, 0, false}, {`412`, 0, false}} {
		var v any
		_ = json.Unmarshal([]byte(c.body), &v)
		if n, ok, _ := testsRun(v); n != c.n || ok != c.ok {
			t.Errorf("%s: %d %v", c.body, n, ok)
		}
	}
}

// Owner marks are read from the board (read-only): a mark landing re-reads the PR and moves merge_state.
func TestBoardMarksMoveMergeState(t *testing.T) {
	w := newWorld(t)
	w.board = map[string]json.RawMessage{"prs": model.MustJSON(map[string]any{})}
	w.pass(0)
	w.board["prs"] = model.MustJSON(map[string]any{"cache-1": map[string]any{"pr": 101, "chiqun_notified": map[string]any{"at": "x"}}})
	if r := w.pass(2 * time.Minute); r.GHCalls != 2 {
		t.Fatalf("a mark change did not re-read: %d calls", r.GHCalls)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("unchanged marks re-read: %d calls", r.GHCalls)
	}
}

// S2: on merge the final entry goes to the inputs form's ledger outbox, once; a changed final entry is sent again;
// an ack (newer than the send, for that record) clears the outbox member and the entry stays; Keep after the merge
// the entry is removed.
func TestLedgerRecordAckAndPrune(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Add(time.Minute).Format(time.RFC3339), w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	rec := string(outboxOf(r)["101"])
	if !strings.Contains(rec, `"kind":"pr-final"`) || !strings.Contains(rec, `"state":"MERGED"`) || !strings.Contains(rec, `"hash"`) || entry(t, r, "pr:101")["state"] != "MERGED" {
		t.Fatalf("record %s", rec)
	}
	var h struct {
		Hash string `json:"hash"`
	}
	_ = json.Unmarshal([]byte(rec), &h)
	sent := w.st.PRs[101].Outbox

	// A comment after the merge (updatedAt moves, the entry's updated_at with it): the record changes, sent again.
	p.updated = w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	rec2 := string(outboxOf(r)["101"])
	if rec2 == "" || rec2 == rec {
		t.Fatal("a changed final entry was not re-sent")
	}
	_ = json.Unmarshal([]byte(rec2), &h)
	if r := w.pass(2 * time.Minute); len(r.Forms) != 0 {
		t.Fatalf("an unchanged record was re-sent: %v", outboxOf(r))
	}

	// Acks: older than the send, or for another record: nothing. Newer and for this record: the member is cleared.
	ackAt := func(at, hash string) {
		w.in["ledger_ack"] = model.MustJSON(map[string]any{"101": map[string]string{"sha": "k1", "path": "p", "at": at, "hash": hash}})
	}
	ackAt(sent, h.Hash)
	if r := w.pass(2 * time.Minute); len(r.Forms) != 0 {
		t.Fatal("cleared on an ack older than the send")
	}
	ackAt(w.now.Format(time.RFC3339), "0000")
	if r := w.pass(2 * time.Minute); len(r.Forms) != 0 {
		t.Fatal("cleared on an ack for another record")
	}
	ackAt(w.now.Format(time.RFC3339), h.Hash)
	r = w.pass(2 * time.Minute)
	if string(outboxOf(r)["101"]) != "null" || len(r.Unset) != 0 || w.st.PRs[101] == nil {
		t.Fatalf("ack: outbox %v unset %v", outboxOf(r), r.Unset)
	}
	if r := w.pass(2 * time.Minute); len(r.Forms) != 0 {
		t.Fatal("an acked record was cleared twice")
	}
	// Keep after the merge, the entry goes.
	if r := w.pass(Keep - 20*time.Minute); len(r.Unset) != 0 {
		t.Fatalf("pruned early: %v", r.Unset)
	}
	r = w.pass(10 * time.Minute)
	if strings.Join(r.Unset, " ") != "pr:101" || w.st.PRs[101] != nil {
		t.Fatalf("not pruned: %v", r.Unset)
	}
}

// A closed issue's entry is removed Keep after it closed.
func TestClosedIssuePruned(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	is := w.gh.items[301]
	is.state, is.closedAt, is.updated = "CLOSED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	if r := w.pass(Keep); strings.Join(r.Unset, " ") != "issue:301" {
		t.Fatalf("issue not pruned: %v", r.Unset)
	}
}

// A round that cannot read its source fails: last_success (what the steward's health timer reads) and the cursor
// do not move; a good round moves both.
func TestFailedReadDoesNotAdvanceLastSuccess(t *testing.T) {
	w := newWorld(t)
	now := w.now
	pl := Plugin{GH: w.gh, Hub: w.hub, Now: func() time.Time { return now }}
	c := model.Critic{Name: "effort-github", Source: model.SourceScout, Scout: &w.watch}
	rep, err := pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c, Inputs: w.in})
	if err != nil || rep.Error != "" || !rep.Critic.Runtime.LastSuccess.Equal(now) || len(rep.Set) != 4 {
		t.Fatalf("good round: %v %q success %v set %d", err, rep.Error, rep.Critic.Runtime.LastSuccess, len(rep.Set))
	}
	c = *rep.Critic
	good, scoutState := c.Runtime.LastSuccess, string(c.Runtime.Scout)
	now = now.Add(2 * time.Minute)
	w.gh.fail = errors.New("HTTP 502")
	rep, err = pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c, Inputs: w.in})
	if err != nil || !strings.Contains(rep.Error, "502") || !rep.Critic.Runtime.LastSuccess.Equal(good) ||
		string(rep.Critic.Runtime.Scout) != scoutState || rep.Critic.Runtime.ConsecutiveFailures != 1 || len(rep.Set) != 0 {
		t.Fatalf("failed round: %q success %v (was %v) failures %d set %d", rep.Error, rep.Critic.Runtime.LastSuccess, good,
			rep.Critic.Runtime.ConsecutiveFailures, len(rep.Set))
	}
	c = *rep.Critic
	now = now.Add(2 * time.Minute)
	w.gh.fail = nil
	rep, _ = pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c, Inputs: w.in})
	if rep.Error != "" || !rep.Critic.Runtime.LastSuccess.Equal(now) || rep.Critic.Runtime.ConsecutiveFailures != 0 {
		t.Fatalf("recovered round: %q success %v", rep.Error, rep.Critic.Runtime.LastSuccess)
	}
}

// Links: closing references, #<n> of four or more digits, and issue URLs; not the PR itself, not "step #1", not an
// HTML entity, not a duplicate.
func TestLinks(t *testing.T) {
	var n ghNode
	n.Number = 108401
	n.Body = "Closes #96000. See #96000 again, step #1, &#1234; x#5555, (#108401) and https://h/bic/aether/issues/97001\n#97002"
	n.Closing.Nodes = append(n.Closing.Nodes, struct {
		Number int `json:"number"`
	}{95000})
	if got := fmt.Sprint(links(n)); got != "[95000 96000 97001 97002]" {
		t.Fatalf("links %s", got)
	}
}
