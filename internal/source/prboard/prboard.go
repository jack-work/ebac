// Package prboard is board sync's source (github-prs, served by ebac-gh): one
// small child form per PR of one author in one repo, holding facts only
// (state, draft, head, checks, reviews, architect verdict, threads,
// mergeability, band jobs with test counts, and the merge checklist computed
// from the policy text), plus the board reconciliation and the ledger record
// on merge or close (board-sync plan, sections A and B, 2026-11-10).
//
// Every pass is a catch-up, never a full pull:
//  1. one GraphQL search for the author's PRs updated at or after the cursor
//     (full facts in the same query, Batch per page);
//  2. one alias batch re-reading only the tracked open PRs whose checks or
//     mergeability were still settling, or whose band jobs or owner marks
//     changed (CI completion does not always bump updatedAt);
//  3. the band hub's jobs (metadata reader) for every tracked head;
//  4. one write per PR form whose facts changed, the board when the
//     reconciliation changed it, the ledger outbox on merge or close.
//
// Owner roles write note.* on the PR forms; this source never does (the core
// refuses it too) and re-asserts its facts every time they change.
package prboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/boardsync"
	"github.com/gluck/ebac/internal/mergecheck"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Batch is how many PRs one GraphQL query reads (search page or alias batch).
const Batch = 50

// CursorSkew re-reads this much before the last pass: the search index lags.
const CursorSkew = 2 * time.Minute

// JobsWindow is how far back the band hub is read for jobs on tracked heads.
const JobsWindow = 72 * time.Hour

// GitHub is what a pass needs from the forge: one GraphQL query per call.
type GitHub interface {
	GraphQL(ctx context.Context, query string) (json.RawMessage, error)
}

// HubJob is one band-hub job as the metadata view shows it.
type HubJob struct {
	ID      int64  `json:"id"`
	Queue   string `json:"queue"`
	State   string `json:"state"`
	Updated int64  `json:"updated"`
	Payload struct {
		Params map[string]json.RawMessage `json:"params"`
	} `json:"payload"`
	Result struct {
		OK *bool `json:"ok"`
	} `json:"result"`
}

func (j HubJob) param(k string) string {
	var s string
	if json.Unmarshal(j.Payload.Params[k], &s) == nil {
		return s
	}
	return ""
}

// Hub is the band hub as a metadata reader sees it.
type Hub interface {
	// Jobs are the jobs updated since a time, newest first.
	Jobs(ctx context.Context, since time.Time) ([]HubJob, error)
	// Tests is a finished test job's count (GET /jobs/<id>/tests, numbers
	// only); ok false while the hub does not answer it (unknown, never a pass).
	Tests(ctx context.Context, id int64) (n int, ok bool, err error)
}

// Tracked is what the state keeps per PR between passes: enough to re-feed
// the reconciliation without re-reading GitHub, and to know what changed.
type Tracked struct {
	Title     string `json:"t"`
	State     string `json:"s"` // OPEN, MERGED, CLOSED
	Draft     bool   `json:"d,omitempty"`
	Additions int    `json:"a,omitempty"`
	Deletions int    `json:"x,omitempty"`
	PState    string `json:"p,omitempty"`
	Head      string `json:"h"`
	Facts     string `json:"f"`           // digest of the fact keys last written
	Jobs      string `json:"j,omitempty"` // digest of the band jobs + counts the checklist last saw
	Marks     string `json:"m,omitempty"` // digest of the owner marks the checklist last saw
	Settling  bool   `json:"r,omitempty"` // checks pending or mergeability unknown: re-read next pass
	Outbox    string `json:"o,omitempty"` // when the ledger record went to the outbox (RFC 3339)
}

// State is the plugin's opaque state (critic runtime.pr_board).
type State struct {
	Cursor string           `json:"cursor,omitempty"`
	PRs    map[int]*Tracked `json:"prs,omitempty"`
	Tests  map[int64]int    `json:"tests,omitempty"` // known test counts by job id (a done job's count never changes)
}

// Pass is one round's inputs.
type Pass struct {
	Watch    model.BoardWatch
	Inputs   map[string]json.RawMessage
	Board    map[string]json.RawMessage
	Children map[string]string
	Now      time.Time
}

// Result is one round's outputs.
type Result struct {
	State     State
	Forms     []plugin.FormWrite
	GHCalls   int
	Changed   []int    // PR forms written
	Judgement []string // reconciliation lines for the Minister
	Log       []string
}

// ChildKey is a PR's child form key.
func ChildKey(n int) string { return fmt.Sprintf("pr-%d", n) }

// Run is one pass.
func Run(ctx context.Context, gh GitHub, hub Hub, prev State, in Pass) (Result, error) {
	st := prev
	if st.PRs == nil {
		st.PRs = map[int]*Tracked{}
	}
	if st.Tests == nil {
		st.Tests = map[int64]int{}
	}
	res := Result{}
	logf := func(f string, a ...any) { res.Log = append(res.Log, fmt.Sprintf(f, a...)) }

	pol := mergecheck.Parse(inputString(in.Inputs["rule"]))
	if len(pol.Steps) == 0 {
		logf("no merge policy on the inputs form (key rule): checklists not computed")
	}

	// 1. the catch-up search.
	start := in.Now.UTC()
	fresh := map[int]PR{}
	after := ""
	for page := 0; page < 20; page++ {
		data, err := gh.GraphQL(ctx, searchQuery(in.Watch.Repo, in.Watch.Author, prev.Cursor, after))
		res.GHCalls++
		if err != nil {
			return res, fmt.Errorf("search: %w", err)
		}
		prs, more, end, err := decodeSearch(data)
		if err != nil {
			return res, err
		}
		for _, p := range prs {
			fresh[p.Number] = p
		}
		if !more {
			break
		}
		after = end
	}

	// 3. band jobs for every tracked or fresh head.
	heads := map[string]bool{}
	for n, t := range st.PRs {
		if p, ok := fresh[n]; ok {
			heads[p.Facts.Oid] = true
		} else if t.State == "OPEN" {
			heads[t.Head] = true
		}
	}
	for _, p := range fresh {
		heads[p.Facts.Oid] = true
	}
	jobs := map[string][]mergecheck.Job{}
	if hub != nil {
		hj, err := hub.Jobs(ctx, in.Now.Add(-JobsWindow))
		if err != nil {
			logf("band hub: %v (band jobs unknown this pass)", err)
		}
		for _, j := range hj {
			sha := j.param("sha")
			if !heads[sha] || !strings.HasPrefix(j.Queue, "aether.") {
				continue
			}
			mj := mergecheck.Job{ID: j.ID, Queue: j.Queue, Tests: j.param("tests"), State: j.State}
			switch {
			case j.State == "done" && j.Result.OK != nil && *j.Result.OK:
				mj.Verdict = "PASS"
			case j.State == "done" || j.State == "failed":
				mj.Verdict = "FAIL"
			case j.State == "cancelled" || j.State == "superseded":
				continue
			}
			if mj.Verdict == "PASS" && contains(mergecheck.TestJobs, j.Queue) {
				if _, known := st.Tests[j.ID]; !known {
					if n, ok, err := hub.Tests(ctx, j.ID); err == nil && ok {
						st.Tests[j.ID] = n
					}
				}
			}
			jobs[sha] = append(jobs[sha], mj)
		}
	}

	marks := boardMarks(in.Board)

	// 2. the re-read: tracked open PRs not in the search whose facts may have moved without an update.
	var reread []int
	for n, t := range st.PRs {
		if _, ok := fresh[n]; ok || t.State != "OPEN" {
			continue
		}
		if t.Settling || t.Jobs != digest(jobsFor(jobs, st.Tests, t.Head)) || t.Marks != digest(marks.of(n)) {
			reread = append(reread, n)
		}
	}
	sort.Ints(reread)
	for i := 0; i < len(reread); i += Batch {
		end := min(i+Batch, len(reread))
		data, err := gh.GraphQL(ctx, batchQuery(in.Watch.Repo, reread[i:end]))
		res.GHCalls++
		if err != nil {
			return res, fmt.Errorf("re-read: %w", err)
		}
		prs, err := decodeBatch(data)
		if err != nil {
			return res, err
		}
		for _, p := range prs {
			fresh[p.Number] = p
		}
	}

	// 4. facts -> PR forms.
	nums := make([]int, 0, len(fresh))
	for n := range fresh {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		p := fresh[n]
		t, known := st.PRs[n]
		if !known && p.Facts.State != "OPEN" {
			continue // closed before we ever tracked it
		}
		if !known {
			t = &Tracked{}
			st.PRs[n] = t
		}
		js := jobsFor(jobs, st.Tests, p.Facts.Oid)
		mk := marks.of(n)
		var steps []mergecheck.Row
		if p.Facts.State == "OPEN" && len(pol.Steps) > 0 {
			steps = mergecheck.Checklist(pol, p.Facts, js.jobs, mk, js.tests, p.Arch)
		}
		pstate := mergecheck.StateOf(&p.Facts, steps)
		keys := factKeys(in.Watch.Repo, p, js, steps, pstate)
		d := digest(keys)
		t.Title, t.State, t.Draft, t.Additions, t.Deletions, t.PState = p.Title, p.Facts.State, p.Facts.Draft, p.Additions, p.Deletions, pstate
		t.Head, t.Jobs, t.Marks = p.Facts.Oid, digest(js), digest(mk)
		t.Settling = settling(p)
		if d != t.Facts {
			set := map[string]json.RawMessage{}
			for k, v := range keys {
				set[k] = v
			}
			res.Forms = append(res.Forms, plugin.FormWrite{Key: ChildKey(n), Set: set})
			res.Changed = append(res.Changed, n)
			t.Facts = d
		}
		if p.Facts.State != "OPEN" && t.Outbox == "" {
			rec := record(in.Watch.Repo, p, keys)
			res.Forms = append(res.Forms, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{
				"ledger_outbox": model.MustJSON(map[string]json.RawMessage{strconv.Itoa(n): rec})}})
			t.Outbox = in.Now.UTC().Format(time.RFC3339)
			logf("PR #%d %s: ledger record to the outbox", n, strings.ToLower(p.Facts.State))
		}
	}

	// The ledger acks: retire a merged or closed PR's form once its record is committed (ack newer than our write).
	acks := ledgerAcks(in.Inputs)
	ledgered := map[int]bool{}
	for n, t := range st.PRs {
		if t.State == "OPEN" || t.Outbox == "" {
			continue
		}
		a, ok := acks[n]
		if !ok || a.At <= t.Outbox {
			continue
		}
		if want := recordHash(n, in.Inputs); want != "" && a.Hash != "" && a.Hash != want {
			logf("PR #%d: ledger ack hash %s differs from the outbox record %s (retiring anyway)", n, a.Hash, want)
		}
		ledgered[n] = true
	}

	// B: the board.
	if in.Board != nil {
		var bprs, stacks map[string]json.RawMessage
		_ = json.Unmarshal(in.Board["prs"], &bprs)
		_ = json.Unmarshal(in.Board["stacks"], &stacks)
		rin := boardsync.Input{Prs: bprs, Stacks: stacks, Needs: needsTexts(in.Inputs), Ledgered: ledgered}
		for n, t := range st.PRs {
			rin.PRs = append(rin.PRs, boardsync.PR{Number: n, Title: t.Title, State: t.State, Draft: t.Draft,
				Additions: t.Additions, Deletions: t.Deletions, PState: t.PState})
		}
		sort.Slice(rin.PRs, func(i, j int) bool { return rin.PRs[i].Number < rin.PRs[j].Number })
		r := boardsync.Reconcile(rin)
		res.Judgement = r.Judgement
		if r.Dirty() {
			res.Forms = append(res.Forms, plugin.FormWrite{Board: true, Set: map[string]json.RawMessage{"prs": model.MustJSON(r.Prs)}})
			logf("board: %d added, %d changed, %d removed", len(r.Added), len(r.Changed), len(r.Removed))
		}
	}

	// Retire after the board no longer cites them (the reconciliation archived the refs this same pass).
	for n := range ledgered {
		res.Forms = append(res.Forms,
			plugin.FormWrite{Key: ChildKey(n), Retire: true},
			plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": model.MustJSON(map[string]any{strconv.Itoa(n): nil})}})
		delete(st.PRs, n)
		logf("PR #%d: ledger record committed, form retired", n)
	}

	// Test counts only for jobs on tracked heads.
	keep := map[int64]bool{}
	for _, js := range jobs {
		for _, j := range js {
			keep[j.ID] = true
		}
	}
	for id := range st.Tests {
		if !keep[id] {
			delete(st.Tests, id)
		}
	}
	st.Cursor = start.Add(-CursorSkew).Format(time.RFC3339)
	res.State = st
	return res, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func settling(p PR) bool {
	if p.Facts.State != "OPEN" {
		return false
	}
	if p.Facts.Mergeable == "UNKNOWN" || p.Facts.MergeState == "UNKNOWN" || p.Checks == "PENDING" || p.Checks == "EXPECTED" {
		return true
	}
	for _, c := range p.Facts.Contexts {
		if c.State == "PENDING" {
			return true
		}
	}
	return false
}

// headJobs are a head's band jobs, newest first, and their known test counts.
type headJobs struct {
	jobs  []mergecheck.Job
	tests map[int64]int
}

func (h headJobs) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Jobs  []mergecheck.Job `json:"jobs"`
		Tests map[int64]int    `json:"tests"`
	}{h.jobs, h.tests})
}

func jobsFor(jobs map[string][]mergecheck.Job, tests map[int64]int, head string) headJobs {
	js := append([]mergecheck.Job(nil), jobs[head]...)
	sort.SliceStable(js, func(i, j int) bool { return js[i].ID > js[j].ID })
	t := map[int64]int{}
	for _, j := range js {
		if n, ok := tests[j.ID]; ok {
			t[j.ID] = n
		}
	}
	return headJobs{jobs: js, tests: t}
}

// factKeys are a PR form's fact keys (the schema the steward renders in stage 2).
func factKeys(repo string, p PR, js headJobs, steps []mergecheck.Row, pstate string) map[string]json.RawMessage {
	k := map[string]json.RawMessage{}
	put := func(key string, v any) { k[key] = model.MustJSON(v) }
	put("pr", p.Number)
	put("repo", repo)
	put("url", p.URL)
	put("title", p.Title)
	put("state", p.Facts.State)
	put("draft", p.Facts.Draft)
	put("head", p.Facts.Oid)
	put("branch", p.Branch)
	put("base", p.Base)
	put("lines", map[string]int{"additions": p.Additions, "deletions": p.Deletions})
	put("times", map[string]string{"created": p.Created, "updated": p.Updated, "ready": p.ReadyAt, "merged": p.MergedAt, "closed": p.ClosedAt})
	put("merge_sha", p.MergeSha)
	put("mergeable", p.Facts.Mergeable)
	put("merge_state", p.Facts.MergeState)
	put("checks", map[string]any{"state": p.Checks, "contexts": p.Facts.Contexts})
	put("reviews", map[string]any{"decision": p.Review, "latest": p.Facts.Reviews})
	put("architect", p.Arch)
	put("unresolved", p.Facts.Unresolved)
	put("closing", p.Closing)
	put("band_jobs", js)
	put("checklist", steps)
	put("pstate", pstate)
	return k
}

// record is the ledger record of a merged or closed PR: the final facts.
func record(repo string, p PR, keys map[string]json.RawMessage) json.RawMessage {
	r := map[string]any{"kind": "pr-final", "repo": repo, "pr": p.Number, "facts": keys}
	b := model.MustJSON(r)
	h := sha256.Sum256(b)
	r["hash"] = hex.EncodeToString(h[:])
	return model.MustJSON(r)
}

// recordHash is the hash of the record this source put in the outbox for n, if it is still there.
func recordHash(n int, inputs map[string]json.RawMessage) string {
	var out map[string]struct {
		Hash string `json:"hash"`
	}
	_ = json.Unmarshal(inputs["ledger_outbox"], &out)
	return out[strconv.Itoa(n)].Hash
}

type ack struct {
	Sha  string `json:"sha"`
	Path string `json:"path"`
	At   string `json:"at"`
	Hash string `json:"hash"`
}

func ledgerAcks(inputs map[string]json.RawMessage) map[int]ack {
	var raw map[string]ack
	_ = json.Unmarshal(inputs["ledger_ack"], &raw)
	out := map[int]ack{}
	for k, a := range raw {
		if n, err := strconv.Atoi(strings.TrimPrefix(k, "#")); err == nil {
			out[n] = a
		}
	}
	return out
}

// needsTexts are the Thinker's needs files by stack key, lowercased: inputs needs {ledger: text, cache: text}.
func needsTexts(inputs map[string]json.RawMessage) map[string]string {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(inputs["needs"], &raw)
	out := map[string]string{}
	for k, v := range raw {
		out[strings.ToLower(k)] = inputString(v)
	}
	return out
}

func inputString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// marks are the owner-written marks per PR the checklist reads: the board's
// prs row naming the PR, then the board item naming it (as minister-control).
type marks struct {
	rows  map[int]map[string]any
	items map[int]map[string]any
}

var prNumRE = regexp.MustCompile(`\d{4,}`)

func boardMarks(board map[string]json.RawMessage) marks {
	m := marks{rows: map[int]map[string]any{}, items: map[int]map[string]any{}}
	var prs, items map[string]map[string]any
	_ = json.Unmarshal(board["prs"], &prs)
	_ = json.Unmarshal(board["items"], &items)
	for _, r := range prs {
		if f, ok := r["pr"].(float64); ok {
			m.rows[int(f)] = r
		}
	}
	for _, it := range items {
		for _, s := range prNumRE.FindAllString(fmt.Sprint(it["pr"]), -1) {
			if n, err := strconv.Atoi(s); err == nil {
				if _, taken := m.items[n]; !taken {
					m.items[n] = it
				}
			}
		}
	}
	return m
}

// markKeys are the only fields the checklist reads from a row or item.
var markKeys = []string{"majors_handled", "majors_note", "marginal", "marginal_reason", "chiqun_notified", "rust_not_ours"}

func (m marks) of(n int) mergecheck.Marks {
	pick := func(src map[string]any) map[string]any {
		out := map[string]any{}
		for _, k := range markKeys {
			if v, ok := src[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	return mergecheck.Marks{Row: pick(m.rows[n]), Item: pick(m.items[n])}
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
