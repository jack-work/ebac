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
	editor     map[string]any   // issue: the last body editor (author{__typename login})
	parent     int              // issue: its parent issue
	subs       []int            // issue: its sub-issues
	readyAt    string           // PR: its last ready-for-review event
	closedBy   []int            // issue: the PRs that close it
	events     []map[string]any // issue: label and state events, oldest first
}

func (p *item) gql() map[string]any {
	labels := []any{}
	for _, l := range p.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	base := map[string]any{"number": p.n, "title": p.title, "url": fmt.Sprintf("https://x/%d", p.n), "state": p.state,
		"updatedAt": p.updated.Format(time.RFC3339), "closedAt": p.closedAt, "author": map[string]any{"login": p.author},
		"createdAt": created.Format(time.RFC3339), "additions": 10, "deletions": 2,
		"labels": map[string]any{"nodes": labels}}
	if p.issue {
		base["__typename"] = "Issue"
		base["body"] = p.body
		if p.editor != nil {
			base["editor"], base["lastEditedAt"] = p.editor, p.updated.Format(time.RFC3339)
		}
		comments := []any{}
		for _, c := range p.comments[max(0, len(p.comments)-50):] { // comments(last:50)
			comments = append(comments, c)
		}
		events := []any{}
		for _, e := range p.events {
			events = append(events, e)
		}
		base["comments"] = map[string]any{"nodes": comments}
		base["timelineItems"] = map[string]any{"nodes": events}
		if p.parent != 0 {
			base["parent"] = map[string]any{"number": p.parent}
		}
		subs := []any{}
		for _, n := range p.subs {
			subs = append(subs, map[string]any{"number": n})
		}
		base["subIssues"] = map[string]any{"nodes": subs}
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
	if p.readyAt != "" {
		base["timelineItems"] = map[string]any{"nodes": []any{map[string]any{"__typename": "ReadyForReviewEvent", "createdAt": p.readyAt}}}
	}
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
	noSubs   bool // a forge without sub-issues: a query naming subIssues is refused
	noIssues bool // issue reads by number fail
}

var (
	searchRE  = regexp.MustCompile(`([ac]\d+): search\(type:ISSUE, first:\d+(?:, after:"([^"]*)")?, query:"((?:[^"\\]|\\.)*)"\)`)
	claimByRE = regexp.MustCompile(`commenter:(\S+) \\"([^\\]*)\\"`)
	labelRE   = regexp.MustCompile(`label:(\S+)`)
	cursorRE  = regexp.MustCompile(`updated:>=(\S+)`)
	authorRE  = regexp.MustCompile(`author:(\S+)`)
	prRE      = regexp.MustCompile(`p(\d+): pullRequest`)
	issueRE   = regexp.MustCompile(`i(\d+): issueOrPullRequest`)
	filesRE   = regexp.MustCompile(`pullRequest\(number:(\d+)\)\{files\(first:100, after:"([^"]*)"\)`)
)

func (g *fakeGH) GraphQL(_ context.Context, q string) (json.RawMessage, error) {
	g.calls++
	g.queries = append(g.queries, q)
	if g.fail != nil {
		return nil, g.fail
	}
	if g.noIssues && strings.Contains(q, "issueOrPullRequest") {
		return nil, fmt.Errorf("graphql: 502")
	}
	if g.noSubs && strings.Contains(q, "subIssues") {
		return nil, fmt.Errorf("graphql: Field 'subIssues' doesn't exist on type 'Issue'")
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
			open := strings.Contains(query, "is:open")
			claims := strings.HasPrefix(m[1], "c")
			var match []*item
			for _, it := range g.items {
				if open && it.state != "OPEN" || !open && it.updated.Before(since) {
					continue
				}
				switch {
				case !claims:
					if it.author != authorRE.FindStringSubmatch(query)[1] {
						continue
					}
				case claimByRE.MatchString(query):
					cm := claimByRE.FindStringSubmatch(query)
					hit := false
					for _, c := range it.comments {
						a, _ := c["author"].(map[string]any)
						body, _ := c["body"].(string)
						hit = hit || it.issue && a != nil && a["login"] == cm[1] && strings.Contains(body, cm[2])
					}
					if !hit {
						continue
					}
				default:
					l := labelRE.FindStringSubmatch(query)[1]
					hit := false
					for _, x := range it.labels {
						hit = hit || it.issue && x == l
					}
					if !hit {
						continue
					}
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
				node := it.gql()
				refs := []any{}
				for _, pn := range it.closedBy {
					pr := g.items[pn]
					fs := []any{}
					for _, f := range pr.files {
						fs = append(fs, map[string]any{"path": f})
					}
					refs = append(refs, map[string]any{"number": pn, "state": pr.state, "mergedAt": pr.mergedAt, "closedAt": pr.closedAt,
						"files": map[string]any{"nodes": fs}})
				}
				node["closedByPullRequestsReferences"] = map[string]any{"nodes": refs}
				nodes = append(nodes, node)
			}
			out[m[1]] = map[string]any{"pageInfo": map[string]any{"hasNextPage": end < len(match), "endCursor": strconv.Itoa(end)}, "nodes": nodes}
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
	jobs   []HubJob
	tests  map[int64]int
	crates map[int64]int // crates_checked by job id (absent: null)
	asked  int           // tests route calls
}

func (h *fakeHub) Jobs(context.Context, time.Time) ([]HubJob, error) { return h.jobs, nil }
func (h *fakeHub) Tests(_ context.Context, id int64) (Counts, error) {
	h.asked++
	var c Counts
	if n, ok := h.tests[id]; ok {
		c.TestsRun = &n
	}
	if n, ok := h.crates[id]; ok {
		c.Crates = &n
	}
	return c, nil
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
	cap   int
	own   []string // the entry keys on the form (Pass.OwnKeys)
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
	r, err := Run(context.Background(), w.gh, w.hub, w.st, Pass{Watch: w.watch, Inputs: w.in, Board: w.board, Now: w.now, Cap: w.cap, OwnKeys: w.own})
	if err == nil {
		w.st = r.State
		// The inputs form's outbox as the core merges the writes into it.
		box := model.ObjectOf(w.in["ledger_outbox"])
		for _, f := range r.Forms {
			for k, v := range model.ObjectOf(f.Set["ledger_outbox"]) {
				if string(v) == "null" {
					delete(box, k)
				} else {
					box[k] = v
				}
			}
		}
		if len(r.Forms) > 0 {
			w.in["ledger_outbox"] = model.MustJSON(box)
		}
	}
	return r, err
}

// commit is the steward committing the outbox member for n and acking it as ledger_committer.py does: the sha256
// of the committed bytes. It returns the hash.
func (w *world) commit(n string) string {
	member := model.ObjectOf(w.in["ledger_outbox"])[n]
	h := CommittedHash(member)
	acks := model.ObjectOf(w.in["ledger_ack"])
	acks[n] = model.MustJSON(map[string]string{"sha": "c0ffee", "path": "prs/" + n + ".json", "at": w.now.Format(time.RFC3339), "hash": h})
	w.in["ledger_ack"] = model.MustJSON(acks)
	return h
}

// created is every fake item's creation time.
var created = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

// vs is a verdict as verdict@head@at (RFC 3339, "" when absent).
func vs(x any) string {
	m, _ := x.(map[string]any)
	at := ""
	if a, ok := m["at"].(float64); ok {
		at = time.Unix(int64(a), 0).UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%v@%v@%s", m["v"], m["h"], at)
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

// The entry is the key map in scout.go (the brief's list plus the steward's read list), short keys, nothing else (no
// bodies), empty values left out.
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
	w.hub.jobs = []HubJob{hubJob(9, "aether.check-ni", headA, "done", true)}
	r := w.pass(0)
	e := entry(t, r, "pr:101")
	want := "a ar au bj c cr fk h lk ln mg ms s t th u" // b (main) and rd (= cr) left out
	var got []string
	for k := range e {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != want {
		t.Fatalf("pr entry keys\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if e["a"] != ours || e["h"] != "aaaaaaaa" || e["th"] != 2.0 || e["mg"] != "MERGEABLE" || fmt.Sprint(e["ln"]) != "[10 2]" ||
		e["fk"] != "code" || e["cr"] != float64(created.Unix()) || e["rd"] != nil || fmt.Sprint(e["bj"]) != "[[9 check-ni PASS]]" {
		t.Fatalf("pr entry %v", e)
	}
	if len(r.Set["pr:101"]) > 600 {
		t.Fatalf("entry is not slim: %d B", len(r.Set["pr:101"]))
	}
	is := entry(t, r, "issue:301")
	got = nil
	for k := range is {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "a dg l s t u" {
		t.Fatalf("issue entry %v", is)
	}
	// m appears once merged; d once a draft.
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	if e := entry(t, w.pass(2*time.Minute), "pr:101"); e["m"] != float64(w.now.Add(-2*time.Minute).Unix()) || e["ms"] != "merged" || e["d"] != nil {
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
	if s := vs(e["ar"]); s != "APPROVE_WITH_NOTES@<nil>@2026-11-10T20:00:00Z" { // at the entry's head: h left out
		t.Fatalf("architect %s", s)
	}
	if s := vs(e["au"]); s != "APPROVE@bbbbbbbb@" {
		t.Fatalf("automaton %s", s)
	}
	if s := vs(e["lk"]); s != "CHANGES_REQUESTED@bbbbbbbb@" {
		t.Fatalf("lukas %s", s)
	}
	p.comments = append(p.comments, map[string]any{"author": map[string]any{"login": "architect-review[bot]"}, "createdAt": "2026-11-10T23:00:00Z",
		"body": "<!-- architect-review-hold -->\npass in progress"})
	p.updated = w.now.Add(time.Minute)
	if s := vs(entry(t, w.pass(2*time.Minute), "pr:101")["ar"]); s != "hold@<nil>@2026-11-10T23:00:00Z" {
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
	if got := strings.Join(setKeys(r), " "); got != "pr:101" || entry(t, r, "pr:101")["h"] != "cccccccc" {
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
	if r.GHCalls != 3 || r.Set["pr:202"] != nil || r.Set["pr:103"] != nil || w.st.Skip[202] != headA[:12] || w.st.Skip[103] != headB[:12] {
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
	if r.GHCalls != 2 || r.Set["issue:302"] == nil || w.st.Missing[1030] == 0 || w.st.Missing[302] != 0 {
		t.Fatalf("linked issue: %d calls, set %v, missing %v", r.GHCalls, setKeys(r), w.st.Missing)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("linked numbers asked again: %d calls", r.GHCalls)
	}
	// Once tracked, an issue's own update is seen by the search.
	w.gh.items[302].state, w.gh.items[302].updated = "CLOSED", w.now.Add(time.Minute)
	if e := entry(t, w.pass(2*time.Minute), "issue:302"); e["s"] != "CLOSED" {
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
	if c := fmt.Sprint(entry(t, r, "pr:101")["c"]); r.GHCalls != 2 || c != "map[p:1 r:SUCCESS]" {
		t.Fatalf("re-read: %d calls, checks %s", r.GHCalls, c)
	}
	if r := w.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("settled PR still re-read: %d calls", r.GHCalls)
	}
}

// ms is the first unmet step of the rule on the inputs form. Today's rule requires check-ni at the head: a check-ni
// finishing moves it (re-read, rewritten); one passed by a descendant counts; the hub's own verdict wins; a cancelled
// job is no job; opt-in jobs (gate-kit) are evidence only (bj, with their tests_run), never a wait.
func TestMergeStateAndBandJobs(t *testing.T) {
	w := newWorld(t)
	ms := func(r Result, n string) any { return entry(t, r, "pr:"+n)["ms"] }
	w.hub.jobs = []HubJob{hubJob(7, "aether.check-ni", headA, "leased", false), hubJob(8, "aether.gate-kit", headA, "failed", false)}
	r0 := w.pass(0)
	if got := ms(r0, "101"); got != "waiting: band jobs" {
		t.Fatalf("check-ni running: %v", got)
	}
	if got := ms(r0, "102"); got != "draft" {
		t.Fatalf("draft: %v", got)
	}
	w.hub.jobs[0] = hubJob(7, "aether.check-ni", headA, "done", true)
	r := w.pass(2 * time.Minute)
	if got := ms(r, "101"); r.GHCalls != 2 || got == "waiting: band jobs" {
		t.Fatalf("check-ni PASS (gate-kit FAIL is opt-in): %d calls, %v", r.GHCalls, got)
	}
	// Passed by a descendant (state superseded, ok false): still a pass. A cancelled newer job is no job.
	pbd := hubJob(9, "aether.check-ni", headA, "superseded", false)
	pbd.Result.PassByDescendant = json.RawMessage(`{"by":123,"sha":"dddd"}`)
	cancelled := hubJob(10, "aether.check-ni", headA, "leased", false)
	cancelled.Result.CancelledBy = json.RawMessage(`"newer"`)
	w.hub.jobs = []HubJob{cancelled, pbd}
	if r := w.pass(2 * time.Minute); r.Set["pr:101"] == nil || ms(r, "101") == "waiting: band jobs" ||
		!strings.Contains(string(r.Set["pr:101"]), `"bj":[[9,"check-ni","PASS"]]`) {
		t.Fatalf("pass by descendant: %s", r.Set["pr:101"])
	}
	// The hub's verdict wins over ok.
	v := hubJob(11, "aether.check-ni", headA, "done", true)
	v.Result.Verdict = "FAIL"
	w.hub.jobs = []HubJob{v}
	if r := w.pass(2 * time.Minute); ms(r, "101") != "waiting: band jobs" {
		t.Fatalf("hub verdict FAIL: %v", ms(r, "101"))
	}
	// Opt-in evidence with its count.
	w.hub.jobs = []HubJob{hubJob(12, "aether.gate-kit", headA, "done", true), hubJob(7, "aether.check-ni", headA, "done", true)}
	w.hub.tests[12] = 412
	r = w.pass(2 * time.Minute)
	if bj := string(r.Set["pr:101"]); !strings.Contains(bj, `[12,"gate-kit","PASS",412]`) || w.st.Tests[12] != 412 {
		t.Fatalf("opt-in evidence: %s", bj)
	}
	w.hub.jobs = nil // the jobs left the hub's window: the count is forgotten
	w.pass(2 * time.Minute)
	if _, kept := w.st.Tests[12]; kept {
		t.Fatalf("a count outlived its job: %v", w.st.Tests)
	}
	// No rule on the inputs form: unknown, said in the log.
	w.in = map[string]json.RawMessage{}
	w.gh.items[101].updated = w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	if ms(r, "101") != "unknown" || !strings.Contains(strings.Join(r.Log, " "), "no merge policy") {
		t.Fatalf("no rule: %v %v", ms(r, "101"), r.Log)
	}
}

// The PR's own diff classifies it (fk, and the band-jobs step): #108334's shape (only a *_tests.rs, check-ni PASS) is
// past band jobs; a docs-only PR needs no job at all; a page of docs with more pages is not known.
func TestFilesKind(t *testing.T) {
	w := newWorld(t)
	w.gh.items[101].files = []string{"aether_rust/crates/aether_grpc/src/effort_stamp_tests.rs"}
	w.gh.items[201].files = []string{"README.md", "aether_rust/docs/decision_judge/x.md"}
	w.hub.jobs = []HubJob{hubJob(7, "aether.check-ni", headA, "done", true)}
	r := w.pass(0)
	if e := entry(t, r, "pr:101"); e["fk"] != "tests" || e["ms"] == "waiting: band jobs" {
		t.Fatalf("tests-only: %v", e)
	}
	if e := entry(t, r, "pr:201"); e["fk"] != "docs" || e["ms"] == "waiting: band jobs" {
		t.Fatalf("docs-only: %v", e)
	}
	more := PR{Files: []string{"a.md"}, FilesMore: "x"}
	if more.KindFiles() != nil || (PR{Files: []string{"a.go"}, FilesMore: "x"}).KindFiles() == nil {
		t.Fatal("a partial page decided more than it can")
	}
}

// The tests route: {"tests_run": n} is a count; 404, null, or a missing key is unknown.
func TestTestsRoute(t *testing.T) {
	str := func(p *int) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprint(*p)
	}
	for body, want := range map[string]string{
		`{"id":7,"state":"done","tests_run":1234,"crates_checked":12}`: "1234 12", `{"id":7,"tests_run":0,"crates_checked":0}`: "0 0",
		`{"id":7,"tests_run":null,"crates_checked":null}`: "? ?", `{"id":7}`: "? ?", `412`: "? ?", `{"crates_checked":"3"}`: "? ?",
	} {
		var v any
		_ = json.Unmarshal([]byte(body), &v)
		if c := countsOf(v); str(c.TestsRun)+" "+str(c.Crates) != want {
			t.Errorf("%s: %s %s", body, str(c.TestsRun), str(c.Crates))
		}
	}
}

// A check-ni PASS that checked 0 crates is no verdict (band jobs not met, the evidence says why); unknown (null)
// reads as a pass, as before the hub counted; a known count passes, is cached (not asked again) and shown in bj.
func TestCheckNiCratesChecked(t *testing.T) {
	w := newWorld(t)
	w.hub.crates = map[int64]int{}
	w.hub.jobs = []HubJob{hubJob(7, "aether.check-ni", headA, "done", true)}
	r := w.pass(0)
	if e := entry(t, r, "pr:101"); e["ms"] == "waiting: band jobs" || fmt.Sprint(e["bj"]) != "[[7 check-ni PASS]]" {
		t.Fatalf("crates unknown: %v %v", e["ms"], e["bj"])
	}
	w.hub.crates[7] = 0
	r = w.pass(2 * time.Minute)
	if e := entry(t, r, "pr:101"); e["ms"] != "waiting: band jobs" || fmt.Sprint(e["bj"]) != "[[7 check-ni PASS <nil> 0]]" || w.st.Crates[7] != 0 {
		t.Fatalf("0 crates: %v %v", e["ms"], e["bj"])
	}
	// A new check-ni that checked 12: band jobs met; the count is asked once.
	w.hub.jobs = []HubJob{hubJob(8, "aether.check-ni", headA, "done", true)}
	w.hub.crates[8] = 12
	r = w.pass(2 * time.Minute)
	if e := entry(t, r, "pr:101"); e["ms"] == "waiting: band jobs" || fmt.Sprint(e["bj"]) != "[[8 check-ni PASS <nil> 12]]" {
		t.Fatalf("12 crates: %v %v", e["ms"], e["bj"])
	}
	asked := w.hub.asked
	w.pass(2 * time.Minute)
	if w.hub.asked != asked {
		t.Fatalf("a known count was asked again: %d -> %d", asked, w.hub.asked)
	}
	if _, kept := w.st.Crates[7]; kept {
		t.Fatal("a count outlived its job")
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
// the steward's ack (the sha256 of the committed record, ledger_committer.py) matches only the member it committed:
// an ack of an older record, or of something else, leaves the member; the ack of the member clears it; the entry
// stays until its record is acked and at least KeepClosed after the merge, however long the ack takes.
func TestLedgerRecordAckAndPrune(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Add(time.Minute).Format(time.RFC3339), w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	rec := string(outboxOf(r)["101"])
	if !strings.Contains(rec, `"kind":"pr-final"`) || !strings.Contains(rec, `"s":"MERGED"`) || !strings.Contains(rec, `"hash"`) || entry(t, r, "pr:101")["s"] != "MERGED" {
		t.Fatalf("record %s", rec)
	}
	w.commit("101") // the steward commits the first record

	// A comment after the merge (updatedAt moves, the entry's u with it): the record changes, sent again.
	p.updated = w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	rec2 := string(outboxOf(r)["101"])
	if rec2 == "" || rec2 == rec {
		t.Fatal("a changed final entry was not re-sent")
	}
	if r := w.pass(2 * time.Minute); len(r.Forms) != 0 {
		t.Fatalf("an unchanged record was re-sent (or the ack of the older one matched): %v", outboxOf(r))
	}
	// Unacked (the ack is the older record's): the entry stays past KeepClosed.
	if r := w.pass(KeepClosed + time.Hour); len(r.Unset)+len(r.Forms) != 0 {
		t.Fatalf("pruned or cleared before the member's ack: %v %v", r.Unset, outboxOf(r))
	}
	w.in["ledger_ack"] = model.MustJSON(map[string]any{"101": map[string]string{"at": w.now.Format(time.RFC3339), "hash": strings.Repeat("0", 64)}})
	if r := w.pass(2 * time.Minute); len(r.Forms)+len(r.Unset) != 0 {
		t.Fatal("cleared on an ack for another record")
	}
	w.commit("101") // the steward commits the record in the outbox now
	r = w.pass(2 * time.Minute)
	if string(outboxOf(r)["101"]) != "null" || strings.Join(r.Unset, " ") != "pr:101" || w.st.PRs[101] != nil {
		t.Fatalf("ack: outbox %v unset %v", outboxOf(r), r.Unset)
	}
}

// The live shape: ledger_committer.py's ack hash is the sha256 of json.dumps(record, indent=2, sort_keys=True,
// ensure_ascii=False) + "\n"; CommittedHash reproduces it on records and acks taken from the live inputs form.
func TestCommittedHashMatchesTheStewardsAcks(t *testing.T) {
	b, err := os.ReadFile("testdata/ledger-acks.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]struct {
		Record json.RawMessage `json:"record"`
		Ack    struct {
			At, Hash, Path, Sha string
		} `json:"ack"`
	}
	if err := json.Unmarshal(b, &m); err != nil || len(m) < 2 {
		t.Fatalf("testdata: %d %v", len(m), err)
	}
	for n, c := range m {
		if got := CommittedHash(c.Record); got != c.Ack.Hash || len(got) != 64 {
			t.Errorf("#%s: %s, the steward acked %s", n, got, c.Ack.Hash)
		}
	}
	// HTML characters stay as they are, numbers keep their text, non-ASCII stays UTF-8 (hashes from Python).
	for rec, want := range map[string]string{
		`{"title": "<a & b>", "n": 1}`:        "404e5a6345c176fdf90353f19f4d6cc0b21c9d812cc3437abc658889511a31e9",
		`{"x": 1.0, "y": [1, 2.5], "z": "é"}`: "f7f8db099a229646d79ece792db82c27a39db5348df1633d8273f08090f4c06c",
	} {
		if got := CommittedHash(json.RawMessage(rec)); got != want {
			t.Errorf("%s: %s, Python %s", rec, got, want)
		}
	}
	// Through a pass: a closed PR whose outbox member is acked with that hash becomes prunable.
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Add(-48*time.Hour).Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	w.commit("101")
	if r := w.pass(2 * time.Minute); strings.Join(r.Unset, " ") != "pr:101" {
		t.Fatalf("an acked closed PR KeepClosed old was not pruned: %v", r.Unset)
	}
}

// An ack within KeepClosed of the merge: the outbox member goes at once, the entry only KeepClosed after the merge.
func TestAckedEntryStaysKeepClosed(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[101]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	w.commit("101")
	if r := w.pass(2 * time.Minute); string(outboxOf(r)["101"]) != "null" || len(r.Unset) != 0 {
		t.Fatalf("early ack: outbox %v unset %v", outboxOf(r), r.Unset)
	}
	if r := w.pass(KeepClosed - 10*time.Minute); len(r.Unset) != 0 {
		t.Fatalf("pruned early: %v", r.Unset)
	}
	if r := w.pass(10 * time.Minute); strings.Join(r.Unset, " ") != "pr:101" {
		t.Fatalf("not pruned: %v", r.Unset)
	}
}

// A closed issue's entry is removed KeepClosed after it closed.
func TestClosedIssuePruned(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	is := w.gh.items[301]
	is.state, is.closedAt, is.updated = "CLOSED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	if r := w.pass(KeepClosed - 5*time.Minute); len(r.Unset) != 0 {
		t.Fatalf("issue pruned early: %v", r.Unset)
	}
	if r := w.pass(10 * time.Minute); strings.Join(r.Unset, " ") != "issue:301" {
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

// A changed filter re-reads every open item: an old PR nobody touched since the cursor, which only the new title
// filter takes in, is tracked on the next pass. An unchanged filter keeps the cursor.
func TestFilterChangeReReads(t *testing.T) {
	w := newWorld(t)
	w.gh.items[104] = &item{n: 104, author: ours, title: "[CACHE][1-3] an old piece", state: "OPEN", head: headC, mergeable: "MERGEABLE",
		updated: w.now.Add(-48 * time.Hour), files: []string{"x/cache.rs"}}
	w.pass(0)
	w.pass(2 * time.Minute)
	if w.st.PRs[104] != nil {
		t.Fatal("tracked before the filter took it in")
	}
	w.watch.TitleMatch = `^\[(Effort|MD1|CACHE)[^\]]*\]`
	q := len(w.gh.queries)
	r := w.pass(2 * time.Minute)
	if r.Set["pr:104"] == nil || !strings.Contains(w.gh.queries[q], "is:open") || !strings.Contains(strings.Join(r.Log, " "), "filter changed") {
		t.Fatalf("filter change: set %v, query %.200s", setKeys(r), w.gh.queries[q])
	}
	q = len(w.gh.queries)
	if r := w.pass(2 * time.Minute); !strings.Contains(w.gh.queries[q], "updated:>=") || len(r.Set) != 0 {
		t.Fatalf("an unchanged filter re-read again: %.200s", w.gh.queries[q])
	}
}

// ready for review: the last ReadyForReviewEvent, else the creation of a PR that never was a draft (rd left out);
// a draft has none.
func TestReadyAt(t *testing.T) {
	w := newWorld(t)
	w.gh.items[101].readyAt = "2026-11-10T12:00:00Z"
	r := w.pass(0)
	if e := entry(t, r, "pr:101"); e["rd"] != float64(unix("2026-11-10T12:00:00Z")) {
		t.Fatalf("ready event: %v", e["rd"])
	}
	if e := entry(t, r, "pr:102"); e["rd"] != nil || e["d"] != 1.0 {
		t.Fatalf("draft: rd %v d %v", e["rd"], e["d"])
	}
	if e := entry(t, r, "pr:201"); e["rd"] != nil || e["cr"] != float64(created.Unix()) {
		t.Fatalf("never a draft: rd %v cr %v", e["rd"], e["cr"])
	}
}

// Issues carry their parent and sub-issues; a forge that does not know sub-issues refuses the query once, the scout
// asks without them from then on (no failed round, no second refusal).
func TestSubIssues(t *testing.T) {
	w := newWorld(t)
	w.gh.items[301].parent, w.gh.items[301].subs = 300, []int{3010, 3011}
	if e := entry(t, w.pass(0), "issue:301"); e["p"] != 300.0 || fmt.Sprint(e["sub"]) != "[3010 3011]" {
		t.Fatalf("tree: %v", e)
	}
	w2 := newWorld(t)
	w2.gh.noSubs = true
	r := w2.pass(0)
	if r.Set["issue:301"] == nil || !w2.st.NoSubs || r.GHCalls != 2 || !strings.Contains(strings.Join(r.Log, " "), "no sub-issues") {
		t.Fatalf("no sub-issues: set %v, calls %d, log %v", setKeys(r), r.GHCalls, r.Log)
	}
	if r := w2.pass(2 * time.Minute); r.GHCalls != 1 {
		t.Fatalf("asked for sub-issues again: %d calls", r.GHCalls)
	}
}

// A PR closed without merging carries its close time (cl) and its retention counts from it: kept until its record is
// acked and KeepClosed after cl. A close GitHub gives no time for counts from when it was first seen closed. A closed
// issue carries cl too.
func TestClosedCarriesItsTime(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	closedAt := w.now.Add(-6 * time.Hour)
	p := w.gh.items[101]
	p.state, p.closedAt, p.updated = "CLOSED", closedAt.Format(time.RFC3339), w.now.Add(time.Minute)
	is := w.gh.items[301]
	is.state, is.closedAt, is.updated = "CLOSED", closedAt.Format(time.RFC3339), w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	e := entry(t, r, "pr:101")
	if e["s"] != "CLOSED" || e["cl"] != float64(closedAt.Unix()) || e["m"] != nil || w.st.PRs[101].Done != closedAt.Unix() {
		t.Fatalf("closed PR: %v done %d", e, w.st.PRs[101].Done)
	}
	if ie := entry(t, r, "issue:301"); ie["cl"] != float64(closedAt.Unix()) {
		t.Fatalf("closed issue: %v", ie)
	}
	w.commit("101")
	// Acked, 6 h after cl: kept (and the closed issue too); 1 d after cl: both gone.
	if r := w.pass(2 * time.Minute); len(r.Unset) != 0 {
		t.Fatalf("pruned 6 h after the close: %v", r.Unset)
	}
	if r := w.pass(KeepClosed - 6*time.Hour); strings.Join(r.Unset, " ") != "pr:101 issue:301" {
		t.Fatalf("not pruned 1 d after cl: %v", r.Unset)
	}
	// No close time from GitHub: counted from the pass that saw it closed.
	w2 := newWorld(t)
	w2.pass(0)
	w2.gh.items[201].state, w2.gh.items[201].updated = "CLOSED", w2.now.Add(time.Minute)
	w2.pass(2 * time.Minute)
	if d := w2.st.PRs[201].Done; d != w2.now.Unix() {
		t.Fatalf("no close time: done %d, want %d", d, w2.now.Unix())
	}
}

// Mergeability recomputing (UNKNOWN) never blocks and never flaps the entry: the last settled value stands in mg and
// for the conflict step; only CONFLICTING (or DIRTY) waits on conflict.
func TestMergeableRecomputing(t *testing.T) {
	w := newWorld(t)
	w.hub.jobs = []HubJob{hubJob(7, "aether.check-ni", headA, "done", true)}
	ms := func(r Result) string { e := entry(t, r, "pr:101"); return fmt.Sprint(e["mg"], " ", e["ms"]) }
	w.gh.items[101].mergeable = "UNKNOWN" // never settled yet: not a wait
	if got := ms(w.pass(0)); got == "UNKNOWN waiting: conflict" || strings.HasSuffix(got, "waiting: conflict") {
		t.Fatalf("unknown, nothing known: %s", got)
	}
	w.gh.items[101].mergeable, w.gh.items[101].updated = "MERGEABLE", w.now.Add(time.Minute)
	before := ms(w.pass(2 * time.Minute))
	if strings.HasPrefix(before, "UNKNOWN") || strings.HasSuffix(before, "waiting: conflict") {
		t.Fatalf("mergeable: %s", before)
	}
	// GitHub recomputes: the entry does not move at all.
	w.gh.items[101].mergeable, w.gh.items[101].updated = "UNKNOWN", w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	if r.Set["pr:101"] == nil || ms(r) != before {
		t.Fatalf("recomputing moved mg or ms: %s -> %s", before, ms(r))
	}
	if w.st.PRs[101].Mergeable != "MERGEABLE" {
		t.Fatalf("last known lost: %q", w.st.PRs[101].Mergeable)
	}
	// A real conflict waits, and stands while GitHub recomputes after it.
	w.gh.items[101].mergeable, w.gh.items[101].updated = "CONFLICTING", w.now.Add(time.Minute)
	if got := ms(w.pass(2 * time.Minute)); got != "CONFLICTING waiting: conflict" {
		t.Fatalf("conflicting: %s", got)
	}
	w.gh.items[101].mergeable, w.gh.items[101].updated = "UNKNOWN", w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	if r.Set["pr:101"] == nil || ms(r) != "CONFLICTING waiting: conflict" {
		t.Fatalf("recomputing after a conflict: %s", ms(r))
	}
}

// A code upgrade that changed the entry shape (State.Schema behind SchemaVersion): one pass re-reads every tracked PR
// and issue by number and rewrites every entry; a
// ledger record already sent and acked is not sent again for the reshape; the next pass is quiet. A failed re-read
// of tracked issues leaves the schema behind, so the next pass tries again.
func TestSchemaUpgradeRewritesEveryEntry(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	p := w.gh.items[201]
	p.state, p.mergedAt, p.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	is := w.gh.items[301] // closed: only a read by number brings it back
	is.state, is.closedAt, is.updated = "CLOSED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	w.commit("201")
	w.pass(2 * time.Minute)
	for i := 0; i < 3; i++ {
		w.pass(2 * time.Minute)
	}
	if !w.st.PRs[201].Acked || w.st.Schema != SchemaVersion {
		t.Fatalf("setup: acked %v schema %d", w.st.PRs[201].Acked, w.st.Schema)
	}
	// The form as an older binary left it: its own shape, its own schema.
	w.st.Schema = 2
	for _, tp := range w.st.PRs {
		tp.Entry = "0ld0ld00"
	}
	w.st.PRs[201].Record = "0ld0ld0ld0ld0ld0" // the record as the old shape hashed it, sent and acked
	for _, ti := range w.st.Issues {
		ti.Entry = "0ld0ld00"
	}
	w.gh.noIssues = true
	r = w.pass(2 * time.Minute)
	if got := strings.Join(setKeys(r), " "); got != "pr:101 pr:102 pr:201" || len(r.Forms) != 0 || w.st.Schema != 2 {
		t.Fatalf("reshape with the issue read failing: set %s, forms %v, schema %d", got, outboxOf(r), w.st.Schema)
	}
	if e := entry(t, r, "pr:201"); e["s"] != "MERGED" || e["m"] == nil || !w.st.PRs[201].Acked {
		t.Fatalf("merged entry %v acked %v", e, w.st.PRs[201].Acked)
	}
	w.gh.noIssues = false
	r = w.pass(2 * time.Minute)
	if got := strings.Join(setKeys(r), " "); got != "issue:301" || len(r.Forms) != 0 || w.st.Schema != SchemaVersion {
		t.Fatalf("reshape retried: set %s, forms %v, schema %d", got, outboxOf(r), w.st.Schema)
	}
	if e := entry(t, r, "issue:301"); e["s"] != "CLOSED" || !w.st.PRs[201].Acked {
		t.Fatalf("merged entry %v acked %v", e, w.st.PRs[201].Acked)
	}
	if r := w.pass(2 * time.Minute); len(r.Set)+len(r.Forms) != 0 || r.GHCalls != 1 {
		t.Fatalf("after the reshape: set %v, %d calls", setKeys(r), r.GHCalls)
	}
}

// An entry on the form the state does not track (left by a lost or older state: an untracked issue, a PR long gone)
// is removed; tracked ones stay; a pass starting from no state removes nothing (it has not read everything yet).
func TestUntrackedEntriesRemoved(t *testing.T) {
	w := newWorld(t)
	w.own = []string{"issue:9999", "pr:5"}
	if r := w.pass(0); strings.Contains(strings.Join(r.Unset, " "), ":") {
		t.Fatalf("removed from a first pass: %v", r.Unset)
	}
	w.own = []string{"issue:301", "issue:9999", "pr:101", "pr:102", "pr:201", "pr:5"}
	r := w.pass(2 * time.Minute)
	if strings.Join(r.Unset, " ") != "issue:9999 pr:5" || !strings.Contains(strings.Join(r.Log, " "), "2 entr(y/ies) no longer tracked removed") {
		t.Fatalf("orphans: unset %v log %v", r.Unset, r.Log)
	}
	w.own = []string{"issue:301", "pr:101", "pr:102", "pr:201"}
	if r := w.pass(2 * time.Minute); len(r.Unset) != 0 {
		t.Fatalf("tracked entries removed: %v", r.Unset)
	}
}
