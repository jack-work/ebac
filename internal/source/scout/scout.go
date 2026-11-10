// Package scout is the GitHub scout (source github-scout, served by ebac-gh):
// one slim entry per relevant PR and issue of a few authors in one repo,
// written as keys pr:<n> and issue:<n> on the scout's own role form, only
// when the entry changes (board-sync v2, S1); and, on merge or close, the
// PR's final entry as a record in the inputs form's ledger outbox, the entry
// kept Keep after it closed and then removed (S2); a material change on an
// issue (body, checklist, labels, state, a human comment; never an edit
// confined to the sync's <!-- plan-sync --> blocks, never a bot) pings the owner role of each open PR whose body links it,
// one line, at most MaxIssuePings a pass (S5).
//
// Every pass is a catch-up, never a full pull:
//  1. ONE GraphQL call: one aliased search per author for PRs and issues
//     updated at or after the cursor, facts inline (a quiet pass is this
//     call and nothing else);
//  2. one alias batch re-reading only the tracked open PRs whose checks or
//     mergeability were still settling, or whose band jobs or owner marks
//     changed (CI completion does not always bump updatedAt);
//  3. the band hub's jobs (metadata reader) for the tracked heads.
//
// The scout never writes GitHub. The board is read only for the owner marks
// the merge checklist reads.
package scout

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

	"github.com/gluck/ebac/internal/mergecheck"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/relevance"
)

// Batch is how many nodes one GraphQL query reads (search page or alias batch).
const Batch = 50

// CursorSkew re-reads this much before the last pass: the search index lags.
const CursorSkew = 2 * time.Minute

// JobsWindow is how far back the band hub is read for jobs on tracked heads.
const JobsWindow = 72 * time.Hour

// Keep is how long an entry stays after its PR merged or closed, or its
// issue closed (S2).
const Keep = 7 * 24 * time.Hour

// MissingRetry is how long a linked number that did not read as an issue
// waits before it is tried again.
const MissingRetry = time.Hour

// MaxFilePages bounds one PR's file listing (100 files a page).
const MaxFilePages = 30

// MaxIssuePings bounds one pass's issue pings; the rest wait in the state's
// queue (at most MaxQueue) for the next passes.
const (
	MaxIssuePings = 6
	MaxQueue      = 50
)

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
	// Tests is a finished test job's count (GET /jobs/<id>/tests, key
	// tests_run); ok false while the hub does not answer it (unknown, never
	// a pass).
	Tests(ctx context.Context, id int64) (n int, ok bool, err error)
}

// Verdict is one reviewer's latest verdict and the head it read.
type Verdict struct {
	Verdict string `json:"verdict"`
	Head    string `json:"head,omitempty"` // 8 hex
	At      string `json:"at,omitempty"`
}

// Checks is the checks summary of a PR's head.
type Checks struct {
	Rollup  string   `json:"rollup"`
	Pass    int      `json:"pass"`
	Fail    int      `json:"fail"`
	Pending int      `json:"pending"`
	Failing []string `json:"failing,omitempty"` // at most three names
}

// PREntry is a PR's entry (key pr:<n>): exactly the board-sync v2 field list.
type PREntry struct {
	N          int      `json:"n"`
	Kind       string   `json:"kind"` // pr
	Author     string   `json:"author"`
	Title      string   `json:"title"`
	State      string   `json:"state"` // OPEN, MERGED, CLOSED
	Draft      bool     `json:"draft"`
	Head8      string   `json:"head8"`
	Base       string   `json:"base"`
	Checks     Checks   `json:"checks"`
	Architect  *Verdict `json:"architect,omitempty"`
	Lukas      *Verdict `json:"lukas,omitempty"`
	Automaton  *Verdict `json:"automaton,omitempty"`
	Threads    int      `json:"threads"` // open review threads
	Mergeable  string   `json:"mergeable"`
	UpdatedAt  string   `json:"updated_at"`
	MergedAt   string   `json:"merged_at,omitempty"`
	MergeState string   `json:"merge_state"` // merged, closed, draft, unknown, ready, or "waiting: <first unmet step>"
}

// IssueEntry is an issue's entry (key issue:<n>).
type IssueEntry struct {
	N         int          `json:"n"`
	Kind      string       `json:"kind"` // issue
	Author    string       `json:"author"`
	Title     string       `json:"title"`
	State     string       `json:"state"` // OPEN, CLOSED
	Labels    []string     `json:"labels"`
	UpdatedAt string       `json:"updated_at"`
	Digests   IssueDigests `json:"digests"`
}

// IssueDigests are what a material change is measured on (S5): the body
// without its checklist, the checklist (both over the body with every
// plan-sync block stripped), the labels (8-byte hex digests), and the count
// of recent human comments (not a bot).
type IssueDigests struct {
	Body          string `json:"body"`
	Checklist     string `json:"checklist"`
	Labels        string `json:"labels"`
	HumanComments int    `json:"human_comments"`
}

// PRKey and IssueKey are the role-form keys of an entry.
func PRKey(n int) string    { return "pr:" + strconv.Itoa(n) }
func IssueKey(n int) string { return "issue:" + strconv.Itoa(n) }

// TrackedPR is what the state keeps per tracked PR between passes.
type TrackedPR struct {
	Author   string `json:"a,omitempty"`
	Title    string `json:"t,omitempty"`
	State    string `json:"s"` // OPEN, MERGED, CLOSED
	Head     string `json:"h"`
	Entry    string `json:"e"`           // digest of the entry last written
	Jobs     string `json:"j,omitempty"` // digest of the band jobs + counts the checklist last saw
	Marks    string `json:"m,omitempty"` // digest of the owner marks the checklist last saw
	Settling bool   `json:"r,omitempty"` // checks pending or mergeability unknown: re-read next pass
	Links    []int  `json:"l,omitempty"` // issue numbers its body links
	Done     string `json:"z,omitempty"` // merged_at or closed_at
	Outbox   string `json:"o,omitempty"` // when the ledger record was last sent (RFC 3339)
	Record   string `json:"rh,omitempty"`
	Acked    string `json:"k,omitempty"` // the record hash the steward acked
}

// TrackedIssue is what the state keeps per tracked issue.
type TrackedIssue struct {
	State     string       `json:"s"`
	Entry     string       `json:"e"`
	Done      string       `json:"z,omitempty"` // closed_at
	D         IssueDigests `json:"d"`
	LastHuman string       `json:"h,omitempty"` // the newest human comment's time
}

// State is the plugin's opaque state (critic runtime.scout).
type State struct {
	Cursor  string                `json:"cursor,omitempty"`
	PRs     map[int]*TrackedPR    `json:"prs,omitempty"`
	Issues  map[int]*TrackedIssue `json:"issues,omitempty"`
	Skip    map[int]string        `json:"skip,omitempty"`    // untracked PR -> the head whose files missed the filter
	Missing map[int]string        `json:"missing,omitempty"` // linked number that is not a readable issue -> when tried
	Tests   map[int64]int         `json:"tests,omitempty"`   // known test counts by job id (a done job's count never changes)
	Queue   []plugin.Ping         `json:"queue,omitempty"`   // issue pings beyond MaxIssuePings, oldest first
}

// Pass is one round's inputs.
type Pass struct {
	Watch  model.ScoutWatch
	Inputs map[string]json.RawMessage
	Board  map[string]json.RawMessage
	Now    time.Time
}

// Result is one round's outputs: the role-form keys to set and unset, the
// inputs form's outbox writes.
type Result struct {
	State   State
	Set     map[string]json.RawMessage
	Unset   []string
	Forms   []plugin.FormWrite
	Pings   []plugin.Ping
	GHCalls int
	Log     []string
}

// matcher is the relevance filter in force for a pass.
type matcher struct {
	f      *relevance.Filter
	labels map[string]bool
}

func (m matcher) all() bool { return m.f.All() && len(m.labels) == 0 }

// quick is a label or title hit.
func (m matcher) quick(title string, labels []string) bool {
	if m.all() || m.f.TitleHit(title) {
		return true
	}
	for _, l := range labels {
		if m.labels[strings.ToLower(l)] {
			return true
		}
	}
	return false
}

// Run is one pass.
func Run(ctx context.Context, gh GitHub, hub Hub, prev State, in Pass) (Result, error) {
	st := prev
	for _, m := range []*map[int]string{&st.Skip, &st.Missing} {
		if *m == nil {
			*m = map[int]string{}
		}
	}
	if st.PRs == nil {
		st.PRs = map[int]*TrackedPR{}
	}
	if st.Issues == nil {
		st.Issues = map[int]*TrackedIssue{}
	}
	if st.Tests == nil {
		st.Tests = map[int64]int{}
	}
	res := Result{Set: map[string]json.RawMessage{}}
	logf := func(f string, a ...any) { res.Log = append(res.Log, fmt.Sprintf(f, a...)) }
	call := func(q string) (json.RawMessage, error) {
		res.GHCalls++
		return gh.GraphQL(ctx, q)
	}

	f, err := relevance.Compile(in.Watch.TitleMatch, in.Watch.Paths)
	if err != nil {
		return res, fmt.Errorf("filter: %w", err)
	}
	m := matcher{f: f, labels: map[string]bool{}}
	for _, l := range in.Watch.Labels {
		m.labels[strings.ToLower(l)] = true
	}
	owners, err := compileOwners(in.Watch.Owners)
	if err != nil {
		return res, err
	}
	pol := mergecheck.Parse(inputString(in.Inputs["rule"]))
	if len(pol.Steps) == 0 {
		logf("no merge policy on the inputs form (key rule): merge_state not computed")
	}

	// 1. the catch-up search: one call, one alias per author.
	start := in.Now.UTC()
	fresh := map[int]PR{}
	freshIssues := map[int]Issue{}
	var after map[int]string
	for page := 0; page < 20; page++ {
		data, err := call(searchQuery(in.Watch.Repo, in.Watch.Authors, prev.Cursor, after))
		if err != nil {
			return res, fmt.Errorf("search: %w", err)
		}
		sp, err := decodeSearch(data)
		if err != nil {
			return res, err
		}
		next := map[int]string{}
		for alias, a := range sp {
			for _, n := range a.Nodes {
				switch {
				case n.Number == 0:
				case n.isPR():
					fresh[n.Number] = toPR(n)
				default:
					freshIssues[n.Number] = toIssue(n)
				}
			}
			if a.PageInfo.HasNextPage {
				i, _ := strconv.Atoi(strings.TrimPrefix(alias, "a"))
				next[i] = a.PageInfo.EndCursor
			}
		}
		if len(next) == 0 {
			break
		}
		after = next
	}

	// Relevance: a PR tracked once stays tracked; a new one is tracked on a label or title hit, else when a changed
	// file matches a path glob (decided once per head; a miss is remembered in skip).
	for n, p := range fresh {
		if st.PRs[n] != nil {
			continue
		}
		hit := m.quick(p.Title, p.Labels)
		if !hit && f.HasPaths() && st.Skip[n] != p.Facts.Oid {
			files, more := p.Files, p.FilesMore
			hit = f.PathHit(files)
			for pg := 0; !hit && more != "" && pg < MaxFilePages; pg++ {
				data, err := call(filesQuery(in.Watch.Repo, n, more))
				if err != nil {
					return res, fmt.Errorf("files of #%d: %w", n, err)
				}
				if files, more, err = decodeFiles(data); err != nil {
					return res, err
				}
				hit = f.PathHit(files)
			}
			if !hit && p.Facts.State == "OPEN" {
				st.Skip[n] = p.Facts.Oid
			}
		}
		if !hit {
			if p.Facts.State != "OPEN" {
				delete(st.Skip, n) // a closed head never moves again: forget it
			}
			delete(fresh, n)
			continue
		}
		delete(st.Skip, n)
	}

	// 3. band jobs for every tracked open or fresh head.
	heads := map[string]bool{}
	for _, t := range st.PRs {
		if t.State == "OPEN" {
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
		data, err := call(batchQuery(in.Watch.Repo, reread[i:min(i+Batch, len(reread))], nil))
		if err != nil {
			return res, fmt.Errorf("re-read: %w", err)
		}
		prs, _, err := decodeBatch(data)
		if err != nil {
			return res, err
		}
		for _, p := range prs {
			fresh[p.Number] = p
		}
	}

	// PR entries.
	for _, n := range sortedKeys(fresh) {
		p := fresh[n]
		t := st.PRs[n]
		if t == nil {
			t = &TrackedPR{}
			st.PRs[n] = t
		}
		if p.Facts.State == "OPEN" && t.State != "" && t.State != "OPEN" {
			t.Done, t.Outbox, t.Record, t.Acked = "", "", "", "" // reopened: a later close is a new record
		}
		js := jobsFor(jobs, st.Tests, p.Facts.Oid)
		mk := marks.of(n)
		e := prEntry(p, pol, js, mk)
		t.Author, t.Title, t.State, t.Head, t.Links = p.Author, p.Title, p.Facts.State, p.Facts.Oid, p.Links
		t.Jobs, t.Marks, t.Settling = digest(js), digest(mk), settling(p)
		if d := digest(e); d != t.Entry {
			res.Set[PRKey(n)] = model.MustJSON(e)
			t.Entry = d
		}
		if p.Facts.State != "OPEN" {
			t.Done = p.MergedAt
			if t.Done == "" {
				t.Done = p.ClosedAt
			}
			rec, h := record(in.Watch.Repo, e)
			if h != t.Record {
				res.Forms = append(res.Forms, outbox(n, rec))
				t.Record, t.Outbox, t.Acked = h, in.Now.UTC().Format(time.RFC3339), ""
				logf("PR #%d %s: ledger record to the outbox", n, strings.ToLower(p.Facts.State))
			}
		}
	}

	// Issues: tracked once, stays tracked; a new one on a label or title hit, or when a tracked PR links it. A linked
	// issue the search did not return (not updated since the cursor) is read once by number.
	linked := map[int]bool{}
	for _, t := range st.PRs {
		for _, k := range t.Links {
			linked[k] = true
		}
	}
	var want []int
	for k := range linked {
		_, isPR := st.PRs[k]
		_, seen := freshIssues[k]
		if isPR || seen || st.Issues[k] != nil || !retry(st.Missing[k], in.Now) {
			continue
		}
		want = append(want, k)
	}
	sort.Ints(want)
	for i := 0; i < len(want); i += Batch {
		part := want[i:min(i+Batch, len(want))]
		for _, k := range part {
			st.Missing[k] = in.Now.UTC().Format(time.RFC3339) // cleared below when it reads as an issue
		}
		data, err := call(batchQuery(in.Watch.Repo, nil, part))
		if err != nil {
			logf("linked issues %v: %v (retried after %s)", part, err, MissingRetry)
			continue
		}
		_, issues, err := decodeBatch(data)
		if err != nil {
			logf("linked issues: %v", err)
			continue
		}
		for _, is := range issues {
			freshIssues[is.Number] = is
		}
	}
	var pings []plugin.Ping
	for _, n := range sortedKeys(freshIssues) {
		is := freshIssues[n]
		delete(st.Missing, n)
		t := st.Issues[n]
		if t == nil {
			if !linked[n] && !m.quick(is.Title, is.Labels) {
				continue
			}
			t = &TrackedIssue{}
			st.Issues[n] = t
		}
		d, lastHuman := issueDigests(is)
		e := IssueEntry{N: n, Kind: "issue", Author: is.Author, Title: is.Title, State: is.State, Labels: is.Labels, UpdatedAt: is.Updated,
			Digests: d}
		if e.Labels == nil {
			e.Labels = []string{}
		}
		if t.Entry != "" { // a baseline is never news
			if what, by := material(t, is, d, lastHuman); len(what) > 0 {
				pings = append(pings, issuePings(st.PRs, owners, is, what, by, logf)...)
			}
		}
		t.D, t.LastHuman = d, lastHuman
		t.State, t.Done = is.State, ""
		if is.State != "OPEN" {
			t.Done = is.ClosedAt
		}
		if d := digest(e); d != t.Entry {
			res.Set[IssueKey(n)] = model.MustJSON(e)
			t.Entry = d
		}
	}
	for k := range st.Missing {
		if !linked[k] {
			delete(st.Missing, k)
		}
	}

	res.Pings = queuePings(&st, pings, logf)

	// S2: the ledger acks. An ack newer than the record's send, for that record, clears its outbox member; the entry
	// stays until Keep after the close.
	acks := ledgerAcks(in.Inputs)
	for _, n := range sortedKeys(st.PRs) {
		t := st.PRs[n]
		if t.State == "OPEN" || t.Record == "" || t.Acked == t.Record {
			continue
		}
		a, ok := acks[n]
		if !ok || !later(a.At, t.Outbox) || a.Hash != "" && a.Hash != t.Record {
			continue
		}
		t.Acked = t.Record
		res.Forms = append(res.Forms, outbox(n, nil))
		logf("PR #%d: ledger record acked (%s), outbox member cleared", n, a.Sha)
	}

	// S2: prune Keep after the close.
	for _, n := range sortedKeys(st.PRs) {
		t := st.PRs[n]
		if t.State == "OPEN" || !older(t.Done, in.Now, Keep) {
			continue
		}
		res.Unset = append(res.Unset, PRKey(n))
		if t.Record != "" && t.Acked != t.Record {
			logf("PR #%d: pruned %s after close with its ledger record unacked (left in the outbox)", n, Keep)
		}
		delete(st.PRs, n)
	}
	for _, n := range sortedKeys(st.Issues) {
		if t := st.Issues[n]; t.State != "OPEN" && older(t.Done, in.Now, Keep) {
			res.Unset = append(res.Unset, IssueKey(n))
			delete(st.Issues, n)
		}
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

func prEntry(p PR, pol mergecheck.Policy, js headJobs, mk mergecheck.Marks) PREntry {
	var steps []mergecheck.Row
	if p.Facts.State == "OPEN" && len(pol.Steps) > 0 {
		steps = mergecheck.Checklist(pol, p.Facts, js.jobs, mk, js.tests, p.Arch)
	}
	e := PREntry{N: p.Number, Kind: "pr", Author: p.Author, Title: p.Title, State: p.Facts.State, Draft: p.Facts.Draft,
		Head8: short(p.Facts.Oid), Base: p.Base, Checks: checksOf(p), Threads: p.Facts.Unresolved, Mergeable: p.Facts.Mergeable,
		UpdatedAt: p.Updated, MergedAt: p.MergedAt, MergeState: mergecheck.StateOf(&p.Facts, steps), Automaton: p.Automaton}
	if a := p.Arch; a != nil {
		v := &Verdict{Verdict: a.Verdict, Head: short(a.Sha), At: a.At}
		if a.Hold {
			v.Verdict, v.At = "hold", a.HoldAt
		}
		e.Architect = v
	}
	for _, r := range p.Facts.Reviews {
		if strings.EqualFold(r.Login, mergecheck.Lukas) {
			e.Lukas = &Verdict{Verdict: r.State, Head: short(r.Oid)}
		}
	}
	return e
}

func checksOf(p PR) Checks {
	c := Checks{Rollup: p.Checks}
	if c.Rollup == "" {
		c.Rollup = "NONE"
	}
	for _, x := range p.Facts.Contexts {
		switch x.State {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			c.Pass++
		case "PENDING":
			c.Pending++
		default:
			c.Fail++
			if len(c.Failing) < 3 {
				c.Failing = append(c.Failing, x.Name)
			}
		}
	}
	return c
}

// outbox is one ledger_outbox member: the record, or nil to remove it.
func outbox(n int, rec json.RawMessage) plugin.FormWrite {
	v := rec
	if v == nil {
		v = json.RawMessage("null")
	}
	return plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{
		"ledger_outbox": model.MustJSON(map[string]json.RawMessage{strconv.Itoa(n): v})}}
}

// record is the ledger record of a merged or closed PR, its final entry, and
// its hash (sha256 of the record without the hash, hex).
func record(repo string, e PREntry) (json.RawMessage, string) {
	r := map[string]any{"kind": "pr-final", "repo": repo, "pr": e.N, "entry": e}
	b := model.MustJSON(r)
	h := sha256.Sum256(b)
	r["hash"] = hex.EncodeToString(h[:])
	return model.MustJSON(r), r["hash"].(string)
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

// later says whether RFC 3339 time a is after b (false when either is unreadable).
func later(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339, a)
	tb, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && err2 == nil && ta.After(tb)
}

// older says whether RFC 3339 time at is more than d before now (false when unreadable).
func older(at string, now time.Time, d time.Duration) bool {
	t, err := time.Parse(time.RFC3339, at)
	return err == nil && now.Sub(t) > d
}

// retry says whether a number last tried at may be tried again (never tried, unreadable, or MissingRetry ago).
func retry(at string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, at)
	return at == "" || err != nil || now.Sub(t) >= MissingRetry
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
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

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
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

func inputString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// marks are the owner-written marks per PR the checklist reads, from the board
// (read-only): the prs row naming the PR, then the item naming it (as
// minister-control).
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

// S5: issue changes to the owners of the PRs linking them.

// owner is a compiled OwnerRule.
type owner struct {
	author string
	title  *regexp.Regexp
	role   string
}

var roleRE = regexp.MustCompile(`^@[0-9a-f]{8,32}$`)

func compileOwners(rules []model.OwnerRule) ([]owner, error) {
	var out []owner
	for i, r := range rules {
		o := owner{author: r.Author, role: r.Role}
		if r.Author == "" || !roleRE.MatchString(r.Role) {
			return nil, fmt.Errorf("owner rule %d: want an author and a role @<hex>, got %q -> %q", i, r.Author, r.Role)
		}
		if r.TitleMatch != "" {
			re, err := regexp.Compile(r.TitleMatch)
			if err != nil {
				return nil, fmt.Errorf("owner rule %d: title_match: %w", i, err)
			}
			o.title = re
		}
		out = append(out, o)
	}
	return out, nil
}

// CheckOwners validates owner rules before they are stored (ebac add-scout).
func CheckOwners(rules []model.OwnerRule) error {
	_, err := compileOwners(rules)
	return err
}

// roleOf is the owner role of a PR: the first rule matching its author (and title), or "".
func roleOf(owners []owner, t *TrackedPR) string {
	for _, o := range owners {
		if strings.EqualFold(o.author, t.Author) && (o.title == nil || o.title.MatchString(t.Title)) {
			return o.role
		}
	}
	return ""
}

var checkRE = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+\[([ xX])\][ \t]*(.*?)[ \t]*$`)

// The plan->issues sync edits as a seat's own login, so its edits are told apart by place, not by author: it
// wraps its part of an issue body between two marker LINES, <!-- plan-sync --> and <!-- /plan-sync --> (the whole
// line, surrounding whitespace trimmed; a marker inside other text is not a marker), and the digests are taken over
// the body with every such block stripped, marker lines included (an unclosed block strips to the end).
const (
	syncOpen  = "<!-- plan-sync -->"
	syncClose = "<!-- /plan-sync -->"
)

// stripSync is the body without its plan-sync blocks.
func stripSync(body string) string {
	var keep []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		switch t := strings.TrimSpace(line); {
		case !in && t == syncOpen:
			in = true
		case in && t == syncClose:
			in = false
		case !in:
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// issueDigests are an issue's digests, and the time of its newest human comment.
func issueDigests(is Issue) (IssueDigests, string) {
	body := stripSync(strings.ReplaceAll(is.Body, "\r\n", "\n"))
	var check []string
	for _, m := range checkRE.FindAllStringSubmatch(body, -1) {
		check = append(check, strings.ToLower(m[1])+"|"+m[2])
	}
	rest := strings.TrimSpace(checkRE.ReplaceAllString(body, ""))
	labels := append([]string(nil), is.Labels...)
	for i := range labels {
		labels[i] = strings.ToLower(labels[i])
	}
	sort.Strings(labels)
	d := IssueDigests{Body: digest(rest), Checklist: digest(check), Labels: digest(labels)}
	last := ""
	for _, c := range is.Comments {
		if human(c.By) {
			d.HumanComments++
			if c.At > last {
				last = c.At
			}
		}
	}
	return d, last
}

// human says whether an actor's change is news: not a bot. An unknown actor (a deleted account, an event beyond
// the timeline read) counts as human: a lost human change is worse than a stray ping.
func human(l *login) bool {
	return l == nil || !l.bot()
}

// material is what changed on a tracked issue since the last pass, by whom, leaving out what a bot did (the sync's
// edits inside its blocks never move the digests).
func material(t *TrackedIssue, is Issue, d IssueDigests, lastHuman string) ([]string, []string) {
	var what, by []string
	add := func(part string, l *login) {
		if !human(l) {
			return
		}
		what = append(what, part)
		who := "?"
		if l != nil {
			who = l.Login
		}
		if !contains(by, who) {
			by = append(by, who)
		}
	}
	if d.Body != t.D.Body {
		add("body", is.BodyBy)
	}
	if d.Checklist != t.D.Checklist {
		add("checklist", is.BodyBy)
	}
	if d.Labels != t.D.Labels {
		add("labels", is.LabelsBy)
	}
	if is.State != t.State {
		add("state "+strings.ToLower(is.State), is.StateBy)
	}
	if lastHuman > t.LastHuman {
		for i := len(is.Comments) - 1; i >= 0; i-- {
			if c := is.Comments[i]; c.At == lastHuman {
				add("comment", c.By)
				break
			}
		}
	}
	return what, by
}

// issuePings are one line per owner role of the open tracked PRs linking the issue.
func issuePings(prs map[int]*TrackedPR, owners []owner, is Issue, what, by []string, logf func(string, ...any)) []plugin.Ping {
	byRole := map[string][]string{}
	var roles []string
	for _, n := range sortedKeys(prs) {
		t := prs[n]
		if t.State != "OPEN" || !containsInt(t.Links, is.Number) {
			continue
		}
		role := roleOf(owners, t)
		if role == "" {
			logf("issue #%d changed: PR #%d (%s) has no owner rule", is.Number, n, t.Author)
			continue
		}
		if _, ok := byRole[role]; !ok {
			roles = append(roles, role)
		}
		byRole[role] = append(byRole[role], "#"+strconv.Itoa(n))
	}
	if len(roles) == 0 {
		logf("issue #%d changed (%s by %s): no open tracked PR links it", is.Number, strings.Join(what, ", "), strings.Join(by, ", "))
		return nil
	}
	title := is.Title
	if len(title) > 60 {
		title = title[:59] + "~"
	}
	var out []plugin.Ping
	for _, role := range roles {
		out = append(out, plugin.Ping{To: role, Text: oneLine(fmt.Sprintf("scout: issue #%d %q changed: %s by %s; linked PR %s; %s",
			is.Number, title, strings.Join(what, ", "), strings.Join(by, ", "), strings.Join(byRole[role], " "), is.URL))})
	}
	return out
}

// queuePings sends at most MaxIssuePings this pass, oldest first; the rest wait in the queue (bounded at MaxQueue,
// the oldest dropped and said).
func queuePings(st *State, fresh []plugin.Ping, logf func(string, ...any)) []plugin.Ping {
	q := append(st.Queue, fresh...)
	if over := len(q) - MaxQueue; over > 0 {
		logf("issue ping queue full: %d oldest dropped", over)
		q = q[over:]
	}
	n := min(len(q), MaxIssuePings)
	send := append([]plugin.Ping(nil), q[:n]...)
	st.Queue = append([]plugin.Ping(nil), q[n:]...)
	if len(st.Queue) == 0 {
		st.Queue = nil
	}
	return send
}

func containsInt(list []int, k int) bool {
	for _, x := range list {
		if x == k {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:399] + "~"
	}
	return s
}
