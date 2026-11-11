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

// SchemaVersion is the shape of the entries this code writes. A state written
// under another one (a code upgrade that changed the entries) re-reads every
// tracked PR and issue once and rewrites every entry, as a filter change does.
// Bump it with any change to PREntry or IssueEntry.
const SchemaVersion = 3

// KeepClosed is the least an entry stays after its PR merged or closed (and,
// for a PR, until its ledger record is acked), or its issue closed (S2).
const KeepClosed = 24 * time.Hour

// The form's size: figaro's FormStateBudget is 192 KB for the whole form,
// and the entries and this state share it. Past FormCapBytes the oldest merged or
// closed entries go first (a PR's only once its ledger record is sent); if
// that is not enough, new entries wait (Held) and the warn role is pinged
// once. FormOverhead is the rest of the form (critic, harness, keys).
const (
	FormCapBytes = 150 << 10
	FormOverhead = 4 << 10
	budgetSlack  = 256
)

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
		OK               *bool           `json:"ok"`
		Verdict          string          `json:"verdict"`
		PassByDescendant json.RawMessage `json:"pass_by_descendant"`
		CancelledBy      json.RawMessage `json:"cancelled_by"`
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
	Tests(ctx context.Context, id int64) (Counts, error)
}

// The entries are read by every studier of the scout's form (steward,
// Knight, Secretary) and the whole form sits under figaro's FormStateBudget,
// so their keys are short and empty values are left out. Times are unix
// seconds. The key map (dashboard-steward read list, 2026-11-11, plus the
// board-sync v2 brief's list):
//
//	pr:<n>    a author, t title, s state (OPEN MERGED CLOSED), d draft (1),
//	          h head8, b base (absent: main), c checks {r rollup, p pass,
//	          f fail, w pending, x failing names}, ar architect / lk lukas /
//	          au automaton {v verdict, h head8 read (absent: h), at (ar only)},
//	          th open threads, mg mergeable, ms merge_state (merged closed draft
//	          unknown ready "waiting: <step>"), u updated, m merged, cl closed
//	          without merging, cr created,
//	          rd ready for review (absent and not d: cr), ln [additions,
//	          deletions], fk files kind (docs tests code), bj band jobs at h
//	          [[job id, queue, verdict, tests_run?, crates_checked?], ...]
//	          (tests_run null when unknown before a known crates_checked)
//	issue:<n> a author, t title, s state (OPEN CLOSED), l labels, u updated, cl closed,
//	          p parent issue, sub sub-issues, dg digests "body.checklist.labels.humans"

// Verdict is one reviewer's latest verdict and the head it read.
type Verdict struct {
	Verdict string `json:"v"`
	Head    string `json:"h,omitempty"` // 8 hex
	At      int64  `json:"at,omitempty"`
}

// Checks is the checks summary of a PR's head.
type Checks struct {
	Rollup  string   `json:"r"`
	Pass    int      `json:"p,omitempty"`
	Fail    int      `json:"f,omitempty"`
	Pending int      `json:"w,omitempty"`
	Failing []string `json:"x,omitempty"` // at most three names
}

// PREntry is a PR's entry (key pr:<n>).
type PREntry struct {
	Author     string   `json:"a"`
	Title      string   `json:"t"`
	State      string   `json:"s"`
	Draft      int      `json:"d,omitempty"` // 1 when draft
	Head8      string   `json:"h"`
	Base       string   `json:"b,omitempty"` // "" is main
	Checks     Checks   `json:"c"`
	Architect  *Verdict `json:"ar,omitempty"`
	Lukas      *Verdict `json:"lk,omitempty"`
	Automaton  *Verdict `json:"au,omitempty"`
	Threads    int      `json:"th,omitempty"`
	Mergeable  string   `json:"mg"`
	MergeState string   `json:"ms"`
	Updated    int64    `json:"u"`
	Merged     int64    `json:"m,omitempty"`
	Closed     int64    `json:"cl,omitempty"` // closed without merging
	Created    int64    `json:"cr,omitempty"`
	Ready      int64    `json:"rd,omitempty"`
	Lines      [2]int   `json:"ln"`
	FilesKind  string   `json:"fk,omitempty"`
	Band       []Band   `json:"bj,omitempty"`
}

// MarshalJSON writes a band job as [id, queue, verdict, tests_run, crates_checked], the trailing unknowns left out
// and an unknown tests_run before a known crates_checked written null.
func (b Band) MarshalJSON() ([]byte, error) {
	a := []any{b.ID, b.Queue, b.Verdict}
	switch {
	case b.Crates != nil:
		var n any
		if b.Tests != nil {
			n = *b.Tests
		}
		a = append(a, n, *b.Crates)
	case b.Tests != nil:
		a = append(a, *b.Tests)
	}
	return json.Marshal(a)
}

// Band is one band job's evidence at the head (h): the newest per queue and
// tests param, as the band-jobs step reads them.
type Band struct {
	ID      int64  `json:"i"`
	Queue   string `json:"q"` // without aether.
	Verdict string `json:"v,omitempty"`
	Tests   *int   `json:"n,omitempty"` // tests_run, when the hub has counted
	Crates  *int   `json:"k,omitempty"` // a check-ni's crates_checked, when the hub has counted
}

// bandOf is the evidence: the newest job per (queue, tests param) of the fast queues, at most six.
func bandOf(js headJobs) []Band {
	seen := map[string]bool{}
	var out []Band
	for _, j := range js.jobs {
		k := j.Queue + "|" + j.Tests
		if seen[k] || !contains(mergecheck.FastJobs, j.Queue) || len(out) == 6 {
			continue
		}
		seen[k] = true
		b := Band{ID: j.ID, Queue: strings.TrimPrefix(j.Queue, "aether."), Verdict: j.Verdict}
		if j.Tests != "" {
			b.Queue += "(" + j.Tests + ")"
		}
		if n, ok := js.tests[j.ID]; ok {
			b.Tests = &n
		}
		b.Crates = j.Crates
		out = append(out, b)
	}
	return out
}

// IssueEntry is an issue's entry (key issue:<n>).
type IssueEntry struct {
	Author  string   `json:"a"`
	Title   string   `json:"t"`
	State   string   `json:"s"`
	Labels  []string `json:"l"`
	Updated int64    `json:"u"`
	Closed  int64    `json:"cl,omitempty"`
	Parent  int      `json:"p,omitempty"`
	Subs    []int    `json:"sub,omitempty"`
	Digests string   `json:"dg"`
}

// IssueDigests are what a material change is measured on (S5): the body
// without its checklist, the checklist (both over the body with every
// plan-sync block stripped), the labels (8-hex digests), and the count of
// recent human comments (not a bot).
type IssueDigests struct {
	Body          string `json:"b"`
	Checklist     string `json:"c"`
	Labels        string `json:"l"`
	HumanComments int    `json:"n,omitempty"`
}

// String is the digests as an entry carries them: body.checklist.labels.humans.
func (d IssueDigests) String() string {
	return fmt.Sprintf("%s.%s.%s.%d", d.Body, d.Checklist, d.Labels, d.HumanComments)
}

// MarshalText and UnmarshalText keep the digests in the state as that string.
func (d IssueDigests) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *IssueDigests) UnmarshalText(b []byte) error {
	parts := strings.Split(string(b), ".")
	if len(parts) != 4 {
		*d = IssueDigests{}
		return nil
	}
	n, _ := strconv.Atoi(parts[3])
	*d = IssueDigests{Body: parts[0], Checklist: parts[1], Labels: parts[2], HumanComments: n}
	return nil
}

// PRKey and IssueKey are the role-form keys of an entry.
func PRKey(n int) string    { return "pr:" + strconv.Itoa(n) }
func IssueKey(n int) string { return "issue:" + strconv.Itoa(n) }

// TrackedPR is what the state keeps per tracked PR between passes, kept small:
// it sits on the same form as the entries.
type TrackedPR struct {
	State     string `json:"s"`              // OPEN, MERGED, CLOSED
	Head      string `json:"h"`              // 12 hex
	Entry     string `json:"e"`              // digest of the entry last written
	Inputs    string `json:"x,omitempty"`    // digest of the band jobs, counts and owner marks the checklist last saw
	Settling  bool   `json:"r,omitempty"`    // checks pending or mergeability unknown: re-read next pass
	Owner     string `json:"o,omitempty"`    // its owner role (S5), resolved when last read
	Mergeable string `json:"mk,omitempty"`   // the last mergeable GitHub settled on (MERGEABLE, CONFLICTING)
	Links     []int  `json:"l,omitempty"`    // issue numbers its body links
	Size      int    `json:"b,omitempty"`    // bytes of its entry on the form
	Held      bool   `json:"held,omitempty"` // new while the form was at its cap: not written yet
	Done      int64  `json:"z,omitempty"`    // merged or closed (unix)
	Outbox    int64  `json:"ot,omitempty"`   // when the ledger record was last sent (unix)
	Record    string `json:"rh,omitempty"`   // that record's hash, 16 hex
	Acked     bool   `json:"k,omitempty"`    // the steward acked that record
}

// TrackedIssue is what the state keeps per tracked issue.
type TrackedIssue struct {
	State     string       `json:"s"`
	Entry     string       `json:"e"`
	Done      int64        `json:"z,omitempty"`  // closed (unix)
	D         IssueDigests `json:"d"`            // as the entry's dg string
	LastHuman int64        `json:"hh,omitempty"` // the newest human comment's time (unix)
	Size      int          `json:"b,omitempty"`
	Held      bool         `json:"held,omitempty"`
}

// State is the plugin's opaque state (critic runtime.scout).
type State struct {
	Cursor  string                `json:"cursor,omitempty"`
	PRs     map[int]*TrackedPR    `json:"prs,omitempty"`
	Issues  map[int]*TrackedIssue `json:"issues,omitempty"`
	Skip    map[int]string        `json:"skip,omitempty"`    // untracked PR -> the head (12 hex) whose files missed the filter
	Missing map[int]int64         `json:"missing,omitempty"` // linked number that is not a readable issue -> when tried (unix)
	Tests   map[int64]int         `json:"tests,omitempty"`   // known test counts by job id (a done job's count never changes)
	Crates  map[int64]int         `json:"crates,omitempty"`  // known check-ni crates checked by job id
	Queue   []plugin.Ping         `json:"queue,omitempty"`   // issue pings beyond MaxIssuePings, oldest first
	Warned  bool                  `json:"warned,omitempty"`  // the warn role was pinged: open entries alone are past the cap
	Warning string                `json:"warning,omitempty"` // digest of the warning key last written
	NoSubs  bool                  `json:"nosubs,omitempty"`  // the forge has no sub-issues: they are not asked for
	Filter  string                `json:"filter,omitempty"`
	Schema  int                   `json:"schema,omitempty"` // the entry shape (SchemaVersion) every entry on the form was written in  // digest of the repo, authors and relevance filter the cursor was read under
}

// Pass is one round's inputs.
type Pass struct {
	Cap    int // the form's cap in bytes (0: FormCapBytes)
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
	added   []string // entry keys written for the first time this pass
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
	if st.Skip == nil {
		st.Skip = map[int]string{}
	}
	if st.Missing == nil {
		st.Missing = map[int]int64{}
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
	if st.Crates == nil {
		st.Crates = map[int64]int{}
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

	// A changed filter (repo, authors, labels, title, paths) re-reads every open item: an untouched PR that the new
	// filter takes in would never come back through updated>=cursor. Its path verdicts were the old filter's.
	cursor := prev.Cursor
	if fd := digest([]any{in.Watch.Repo, in.Watch.Authors, in.Watch.Labels, in.Watch.TitleMatch, in.Watch.Paths}); fd != st.Filter {
		if st.Filter != "" {
			logf("filter changed: full re-read of every open PR and issue")
		}
		cursor, st.Filter, st.Skip = "", fd, map[int]string{}
	}
	// A new entry shape: every tracked PR and issue is read again by number (the re-read and the tracked-issue batch)
	// and its entry rewritten (its old-shape digest never matches). Ledger records already sent are not sent again
	// for the reshape alone.
	migrating := st.Schema != SchemaVersion
	if migrating {
		logf("entry schema %d -> %d: re-reading and rewriting every entry", st.Schema, SchemaVersion)
	}

	// 1. the catch-up search: one call, one alias per author.
	start := in.Now.UTC()
	fresh := map[int]PR{}
	freshIssues := map[int]Issue{}
	var after map[int]string
	for page := 0; page < 20; page++ {
		data, err := call(searchQuery(in.Watch.Repo, in.Watch.Authors, cursor, after, !st.NoSubs))
		if err != nil && !st.NoSubs && noSubIssues(err) {
			// A forge without sub-issues: say so once, ask without them from now on.
			st.NoSubs = true
			logf("the forge has no sub-issues (%v): issue entries carry no p/sub", err)
			data, err = call(searchQuery(in.Watch.Repo, in.Watch.Authors, cursor, after, false))
		}
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
		if !hit && f.HasPaths() && st.Skip[n] != cut(p.Facts.Oid, 12) {
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
				st.Skip[n] = cut(p.Facts.Oid, 12)
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
		heads[cut(p.Facts.Oid, 12)] = true
	}
	jobs := map[string][]mergecheck.Job{}
	if hub != nil {
		hj, err := hub.Jobs(ctx, in.Now.Add(-JobsWindow))
		if err != nil {
			logf("band hub: %v (band jobs unknown this pass)", err)
		}
		for _, j := range hj {
			sha := cut(j.param("sha"), 12)
			if !heads[sha] || !strings.HasPrefix(j.Queue, "aether.") {
				continue
			}
			if present(j.Result.CancelledBy) || j.State == "cancelled" {
				continue
			}
			// As minister-control graph.py: the hub's verdict, else PASS when ok or passed by a descendant (a
			// check-ni a descendant head passed counts at this head), else FAIL when failed or dead, else none yet.
			mj := mergecheck.Job{ID: j.ID, Queue: j.Queue, Tests: j.param("tests"), State: j.State, Verdict: j.Result.Verdict}
			switch {
			case mj.Verdict != "":
			case j.Result.OK != nil && *j.Result.OK || present(j.Result.PassByDescendant):
				mj.Verdict = "PASS"
			case j.State == "failed" || j.State == "dead":
				mj.Verdict = "FAIL"
			}
			if mj.Verdict == "PASS" {
				_, knownT := st.Tests[j.ID]
				_, knownC := st.Crates[j.ID]
				wantT := contains(mergecheck.TestJobs, j.Queue) && !knownT
				wantC := j.Queue == "aether.check-ni" && !knownC
				if wantT || wantC {
					if c, err := hub.Tests(ctx, j.ID); err == nil {
						if c.TestsRun != nil {
							st.Tests[j.ID] = *c.TestsRun
						}
						if c.Crates != nil && j.Queue == "aether.check-ni" {
							st.Crates[j.ID] = *c.Crates
						}
					}
				}
				if n, ok := st.Crates[j.ID]; ok {
					mj.Crates = &n
				}
			}
			jobs[sha] = append(jobs[sha], mj)
		}
	}
	marks := boardMarks(in.Board)

	// 2. the re-read: tracked open PRs not in the search whose facts may have moved without an update.
	var reread []int
	for n, t := range st.PRs {
		if _, ok := fresh[n]; ok || t.State != "OPEN" && !migrating {
			continue
		}
		if migrating || t.Settling || t.Held || t.Inputs != digest([]any{jobsFor(jobs, st.Tests, t.Head), marks.of(n)}) {
			reread = append(reread, n)
		}
	}
	sort.Ints(reread)
	for i := 0; i < len(reread); i += Batch {
		data, err := call(batchQuery(in.Watch.Repo, reread[i:min(i+Batch, len(reread))], nil, false))
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
			t.Done, t.Outbox, t.Record, t.Acked = 0, 0, "", false // reopened: a later close is a new record
		}
		// GitHub recomputing mergeability (UNKNOWN) is no news: the last settled value stands, in the entry and for
		// the conflict step.
		switch p.Facts.Mergeable {
		case "MERGEABLE", "CONFLICTING":
			t.Mergeable = p.Facts.Mergeable
		default:
			p.Facts.LastMergeable = t.Mergeable
		}
		head := cut(p.Facts.Oid, 12)
		js := jobsFor(jobs, st.Tests, head)
		mk := marks.of(n)
		e := prEntry(p, pol, js, mk)
		t.State, t.Head, t.Links, t.Owner = p.Facts.State, head, p.Links, roleOf(owners, p.Author, p.Title)
		if p.Facts.State != "OPEN" {
			t.Mergeable = "" // only an open PR's mergeability is asked again
		}
		t.Inputs, t.Settling = digest([]any{js, mk}), settling(p)
		if d := digest(e); d != t.Entry {
			raw := model.MustJSON(e)
			if t.Entry == "" {
				res.added = append(res.added, PRKey(n))
			}
			res.Set[PRKey(n)] = raw
			t.Entry, t.Size, t.Held = d, entrySize(PRKey(n), raw), false
		}
		if p.Facts.State != "OPEN" {
			done := unix(p.MergedAt)
			if done == 0 {
				done = unix(p.ClosedAt)
			}
			switch {
			case done != 0:
				t.Done = done
			case t.Done == 0:
				t.Done = in.Now.Unix() // no close time from GitHub: retention counts from when it was first seen closed
			}
			rec, h := record(in.Watch.Repo, n, e)
			if migrating && t.Record != "" {
				t.Record = h // the reshape alone is no new record: the one sent stands (and its ack)
			}
			if h != t.Record {
				res.Forms = append(res.Forms, outbox(n, rec))
				t.Record, t.Outbox, t.Acked = h, in.Now.Unix(), false
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
	var tracked, want []int
	for k, t := range st.Issues {
		if _, seen := freshIssues[k]; !seen && (t.Held || migrating) { // held back at the cap, or a reshape: read again
			tracked = append(tracked, k)
		}
	}
	for k := range linked {
		_, isPR := st.PRs[k]
		_, seen := freshIssues[k]
		if isPR || seen || st.Issues[k] != nil || !retry(st.Missing[k], in.Now) {
			continue
		}
		want = append(want, k)
	}
	sort.Ints(tracked)
	sort.Ints(want)
	reshaped := true // every tracked issue read again (when migrating)
	// Tracked issues in their own batches: a bad linked number fails its whole batch, and must not fail theirs.
	for li, list := range [][]int{tracked, want} {
		what := []string{"tracked issues", "linked issues"}[li]
		for i := 0; i < len(list); i += Batch {
			part := list[i:min(i+Batch, len(list))]
			for _, k := range part {
				if st.Issues[k] == nil {
					st.Missing[k] = in.Now.Unix() // cleared below when it reads as an issue
				}
			}
			data, err := call(batchQuery(in.Watch.Repo, nil, part, !st.NoSubs))
			var issues []Issue
			if err == nil {
				_, issues, err = decodeBatch(data)
			}
			if err != nil {
				logf("%s %v: %v (retried later)", what, part, err)
				if li == 0 {
					reshaped = false
				}
				continue
			}
			for _, is := range issues {
				freshIssues[is.Number] = is
			}
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
		e := IssueEntry{Author: is.Author, Title: is.Title, State: is.State, Labels: is.Labels, Updated: unix(is.Updated),
			Parent: is.Parent, Subs: is.Subs, Digests: d.String()}
		if e.Labels == nil {
			e.Labels = []string{}
		}
		if is.State == "CLOSED" {
			e.Closed = unix(is.ClosedAt)
		}
		if t.Entry != "" { // a baseline is never news
			if what, by := material(t, is, d, lastHuman); len(what) > 0 {
				pings = append(pings, issuePings(st.PRs, is, what, by, logf)...)
			}
		}
		t.D, t.LastHuman = d, unix(lastHuman)
		if is.State == "OPEN" {
			t.Done = 0
		} else if done := unix(is.ClosedAt); done != 0 {
			t.Done = done
		} else if t.Done == 0 {
			t.Done = in.Now.Unix() // no close time: counted from when it was first seen closed
		}
		t.State = is.State
		if d := digest(e); d != t.Entry {
			raw := model.MustJSON(e)
			if t.Entry == "" {
				res.added = append(res.added, IssueKey(n))
			}
			res.Set[IssueKey(n)] = raw
			t.Entry, t.Size, t.Held = d, entrySize(IssueKey(n), raw), false
		}
	}
	for k := range st.Missing {
		if !linked[k] {
			delete(st.Missing, k)
		}
	}

	res.Pings = queuePings(&st, pings, logf)

	// S2: the ledger acks. An ack newer than the record's send, for that record, clears its outbox member.
	acks := ledgerAcks(in.Inputs)
	for _, n := range sortedKeys(st.PRs) {
		t := st.PRs[n]
		if t.State == "OPEN" || t.Record == "" || t.Acked {
			continue
		}
		a, ok := acks[n]
		if !ok || unix(a.At) <= t.Outbox || a.Hash != "" && !strings.HasPrefix(a.Hash, t.Record) {
			continue
		}
		t.Acked = true
		res.Forms = append(res.Forms, outbox(n, nil))
		logf("PR #%d: ledger record acked (%s), outbox member cleared", n, a.Sha)
	}

	// S2: a merged or closed PR's entry stays until its ledger record is acked, and at least KeepClosed; a closed
	// issue's KeepClosed.
	now := in.Now.Unix()
	for _, n := range sortedKeys(st.PRs) {
		if t := st.PRs[n]; t.State != "OPEN" && t.Acked && now-t.Done > int64(KeepClosed/time.Second) {
			res.Unset = append(res.Unset, PRKey(n))
			delete(st.PRs, n)
		}
	}
	for _, n := range sortedKeys(st.Issues) {
		if t := st.Issues[n]; t.State != "OPEN" && t.Done > 0 && now-t.Done > int64(KeepClosed/time.Second) {
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
	for id := range st.Crates {
		if !keep[id] {
			delete(st.Crates, id)
		}
	}
	st.Cursor = start.Add(-CursorSkew).Format(time.RFC3339)
	if reshaped {
		st.Schema = SchemaVersion // else the next pass reshapes again
	}
	budget(&st, &res, in, logf)
	res.State = st
	return res, nil
}

func prEntry(p PR, pol mergecheck.Policy, js headJobs, mk mergecheck.Marks) PREntry {
	var steps []mergecheck.Row
	if p.Facts.State == "OPEN" && len(pol.Steps) > 0 {
		steps = mergecheck.Checklist(pol, p.Facts, js.jobs, mk, js.tests, p.Arch, p.KindFiles())
	}
	head8 := short(p.Facts.Oid)
	at := func(sha string) string { // a verdict at the entry's own head leaves h out
		if s := short(sha); s != head8 {
			return s
		}
		return ""
	}
	e := PREntry{Author: p.Author, Title: p.Title, State: p.Facts.State, Head8: head8, Base: p.Base,
		Checks: checksOf(p), Threads: p.Facts.Unresolved, Mergeable: p.Facts.Mergeable, MergeState: mergecheck.StateOf(&p.Facts, steps),
		Automaton: p.Automaton, Updated: unix(p.Updated), Merged: unix(p.MergedAt), Created: unix(p.Created), Ready: unix(p.ReadyAt),
		Lines: [2]int{p.Additions, p.Deletions}, FilesKind: mergecheck.KindOf(p.KindFiles()), Band: bandOf(js)}
	if p.Facts.State == "CLOSED" {
		e.Closed = unix(p.ClosedAt)
	}
	if p.Facts.LastMergeable != "" {
		e.Mergeable = p.Facts.LastMergeable // recomputing: the last settled value stands
	}
	if e.Base == "main" {
		e.Base = ""
	}
	if e.Ready == e.Created && !p.Facts.Draft {
		e.Ready = 0 // ready when created
	}
	if p.Facts.Draft {
		e.Draft = 1
	}
	if v := e.Automaton; v != nil {
		e.Automaton = &Verdict{Verdict: v.Verdict, Head: at(v.Head)}
	}
	if a := p.Arch; a != nil {
		v := &Verdict{Verdict: a.Verdict, Head: at(a.Sha), At: unix(a.At)}
		if a.Hold {
			v.Verdict, v.At = "hold", unix(a.HoldAt)
		}
		e.Architect = v
	}
	for _, r := range p.Facts.Reviews {
		if strings.EqualFold(r.Login, mergecheck.Lukas) {
			e.Lukas = &Verdict{Verdict: r.State, Head: at(r.Oid)}
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
// its hash (sha256 of the record without the hash, hex; the state keeps 16
// hex of it and an ack matches on that prefix).
func record(repo string, n int, e PREntry) (json.RawMessage, string) {
	r := map[string]any{"kind": "pr-final", "repo": repo, "pr": n, "entry": e}
	b := model.MustJSON(r)
	h := sha256.Sum256(b)
	r["hash"] = hex.EncodeToString(h[:])
	return model.MustJSON(r), r["hash"].(string)[:16]
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
func unix(rfc3339 string) int64 {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// retry says whether a number last tried at may be tried again (never tried, or MissingRetry ago).
func retry(at int64, now time.Time) bool {
	return at == 0 || now.Unix()-at >= int64(MissingRetry/time.Second)
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// entrySize is what an entry costs on the form: its key and its value.
func entrySize(key string, raw json.RawMessage) int { return len(key) + len(raw) + 4 }

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

// digest is 8 hex of a value's sha256: enough to tell a change, small on the form.
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:4])
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
func roleOf(owners []owner, author, title string) string {
	for _, o := range owners {
		if strings.EqualFold(o.author, author) && (o.title == nil || o.title.MatchString(title)) {
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
	if unix(lastHuman) > t.LastHuman {
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
func issuePings(prs map[int]*TrackedPR, is Issue, what, by []string, logf func(string, ...any)) []plugin.Ping {
	byRole := map[string][]string{}
	var roles []string
	for _, n := range sortedKeys(prs) {
		t := prs[n]
		if t.State != "OPEN" || !containsInt(t.Links, is.Number) {
			continue
		}
		role := t.Owner
		if role == "" {
			logf("issue #%d changed: PR #%d has no owner rule", is.Number, n)
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

// budget keeps the form under its cap (FormCapBytes). Past it, the oldest merged or closed
// entries go first (a PR's only once its ledger record is sent: the record
// outlives the entry); if that is not enough, entries new this pass wait
// (Held, re-read on later passes). The warning key says so while it lasts,
// and the warn role is pinged once when open entries alone pass the cap.
func budget(st *State, res *Result, in Pass, logf func(string, ...any)) {
	limit := in.Cap
	if limit <= 0 {
		limit = FormCapBytes
	}
	// The bookkeeping of a pruning pass (held flags, the warning digest) adds a little to the state: aim that much under.
	size := func() (int, int) { t, o := FormSize(st); return t + budgetSlack, o }
	total, open := size()
	var pruned, held []string
	if total > limit {
		type cand struct {
			key  string
			done int64
			drop func()
		}
		var cs []cand
		for n, t := range st.PRs {
			if t.State != "OPEN" && t.Record != "" && t.Size > 0 {
				n := n
				cs = append(cs, cand{PRKey(n), t.Done, func() { delete(st.PRs, n) }})
			}
		}
		for n, t := range st.Issues {
			if t.State != "OPEN" && t.Size > 0 {
				n := n
				cs = append(cs, cand{IssueKey(n), t.Done, func() { delete(st.Issues, n) }})
			}
		}
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].done != cs[j].done {
				return cs[i].done < cs[j].done
			}
			return cs[i].key < cs[j].key
		})
		for _, c := range cs {
			if total, _ = size(); total <= limit {
				break
			}
			c.drop()
			delete(res.Set, c.key)
			res.Unset = append(res.Unset, c.key)
			pruned = append(pruned, c.key)
		}
	}
	if total, _ = size(); total > limit {
		for i := len(res.added) - 1; i >= 0 && total > limit; i-- {
			key := res.added[i]
			kind, num, _ := strings.Cut(key, ":")
			n, _ := strconv.Atoi(num)
			if kind == "pr" {
				if t := st.PRs[n]; t != nil {
					t.Entry, t.Size, t.Held = "", 0, true
				}
			} else if t := st.Issues[n]; t != nil {
				t.Entry, t.Size, t.Held = "", 0, true
			}
			delete(res.Set, key)
			held = append(held, key)
			total, _ = size()
		}
	}
	total, open = size()
	warning := ""
	if len(pruned) > 0 || len(held) > 0 || total > limit {
		warning = fmt.Sprintf("form ~%d KB, cap %d KB: %d merged/closed entr(y/ies) pruned oldest first, %d new held back this pass",
			total>>10, limit>>10, len(pruned), len(held))
		logf("%s (pruned %v, held %v)", warning, pruned, held)
	}
	if d := digest(warning); d != st.Warning {
		if warning == "" {
			res.Unset = append(res.Unset, "warning")
		} else {
			res.Set["warning"] = model.MustJSON(map[string]string{"at": in.Now.UTC().Format(time.RFC3339), "text": warning})
		}
		st.Warning = d
	}
	switch {
	case open > limit && !st.Warned && in.Watch.WarnTo != "":
		res.Pings = append(res.Pings, plugin.Ping{To: in.Watch.WarnTo, Text: oneLine(fmt.Sprintf(
			"scout: open entries alone are ~%d KB, past the %d KB form cap; new entries are held back (key warning)", open>>10, limit>>10))})
		st.Warned = true
	case open <= limit*9/10:
		st.Warned = false
	}
}

// present says whether a JSON value is there and truthy (not null, false, 0, "" or empty).
func present(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", `""`, "{}", "[]":
		return false
	}
	return true
}

// FormSize is what the scout puts on its form: every entry, its own state
// and FormOverhead; and the open entries alone.
func FormSize(st *State) (total, open int) {
	for _, t := range st.PRs {
		total += t.Size
		if t.State == "OPEN" {
			open += t.Size
		}
	}
	for _, t := range st.Issues {
		total += t.Size
		if t.State == "OPEN" {
			open += t.Size
		}
	}
	return total + len(model.MustJSON(st)) + FormOverhead, open
}
