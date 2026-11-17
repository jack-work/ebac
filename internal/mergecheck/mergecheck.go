// Package mergecheck is the PR merge checklist of the Commander's rule.merge_ready (role form @2ea091e5, Boss
// 11-16; it replaced RULE-chiqun-merge-criteria.md): one row per step, each evaluated from facts, and one state per
// PR (draft, "waiting: <label>" of the first unmet step, ready, merged, closed, unknown, or the architect's
// "blocked: architect RC at <h8>").
//
// ready is MERGE READY by the facts: one general bot approval at the head (or a same-tree head), no red of ours, no
// conflict. Checks are not waited on. Whether a reviewer's stated condition is open is the Minister's judgment
// (ready.<pr> on its form, rule.queue_protocol): the needs-merge table follows ready.<pr>, not this state.
//
// The rule is prose, so its steps live here (Steps). Their ids and labels are minister-control states.py's
// (1b629ed), whose local checklist covers the PRs the scout does not carry; RuleSHA is the rule text they encode
// (sha256[:16]), and the board flags a change of it. Pure: no I/O.
package mergecheck

import (
	"fmt"
	"strings"
	"time"
)

// RuleSHA is sha256[:16] of the rule.merge_ready text Steps encode.
const RuleSHA = "c951bc0b4047d33e"

// Logins and pipelines the steps read.
const (
	Ilya      = "Ilya-Grebnov" // the architect-review bot's APPROVE also lands as a review under this login
	Lukas     = "lukas"
	Automaton = "automaton"
	Runtime   = "Aether Runtime PR Pipeline"
	Rust      = "Aether Rust PR Pipeline"
)

// Bots are the general review bots whose APPROVED review counts (rule.merge_ready).
var Bots = []string{Ilya, Lukas, Automaton}

// FastJobs are the band-hub queues read at a head (evidence; a check-ni FAIL is a red). TestJobs run tests (the
// hub's /jobs/<id>/tests counts them).
var (
	FastJobs = []string{"aether.check-ni", "aether.gate-kit", "aether.gate-ext", "aether.gate-services", "aether.clippy-pr"}
	TestJobs = []string{"aether.gate-kit", "aether.gate-ext", "aether.gate-services"}
)

// Step is one step of the rule.
type Step struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Steps are rule.merge_ready's, in order. conditions has no evaluator: it never blocks (the Minister's ready.<pr>).
var Steps = []Step{
	{"bot-approval", "bot approval"},
	{"no-red", "no red of ours"},
	{"conflict", "conflict"},
	{"conditions", "reviewer conditions"},
}

// KindOf is what a PR's changed files are: docs when every file is a doc, tests when every file is a doc or a test,
// else code; "" when the files are not known (nil).
func KindOf(files []string) string {
	if files == nil {
		return ""
	}
	doc := func(p string) bool {
		return strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".txt") || strings.HasSuffix(p, ".rst") || strings.Contains("/"+p, "/docs/")
	}
	test := func(p string) bool {
		for _, s := range []string{"_tests.rs", "_test.rs", "_test.py", ".test.ts"} {
			if strings.HasSuffix(p, s) {
				return true
			}
		}
		return strings.Contains("/"+p, "/tests/") || strings.Contains("/"+p, "/test_")
	}
	all := func(f func(string) bool) bool {
		for _, p := range files {
			if !f(p) {
				return false
			}
		}
		return len(files) > 0
	}
	switch {
	case all(doc):
		return "docs"
	case all(func(p string) bool { return doc(p) || test(p) }):
		return "tests"
	}
	return "code"
}

// Context is one CI status context at the PR head.
type Context struct {
	Name  string `json:"name"`
	State string `json:"state"` // SUCCESS, FAILURE, ERROR, PENDING, ...
	Build string `json:"build,omitempty"`
}

// Review is a latest opinionated review.
type Review struct {
	Login string `json:"login"`
	State string `json:"state"`
	Oid   string `json:"oid,omitempty"`
	Tree  string `json:"tree,omitempty"` // the reviewed commit's tree
	At    string `json:"at,omitempty"`   // submittedAt (RFC 3339, UTC)
}

// Facts are a PR's GitHub facts.
type Facts struct {
	Oid        string `json:"oid"`
	Tree       string `json:"tree,omitempty"` // the head's tree ("" not known)
	State      string `json:"state"`          // OPEN, MERGED, CLOSED
	Draft      bool   `json:"draft"`
	Heavy      bool   `json:"heavy"` // full facts (checks, threads, reviews) were read
	Mergeable  string `json:"mergeable,omitempty"`
	MergeState string `json:"merge_state,omitempty"`
	Review     string `json:"review,omitempty"` // GitHub's review decision
	// LastMergeable is the last mergeable GitHub settled on, read while it
	// recomputes (UNKNOWN).
	LastMergeable string    `json:"last_mergeable,omitempty"`
	Unresolved    int       `json:"unresolved"`
	Contexts      []Context `json:"contexts,omitempty"`
	Reviews       []Review  `json:"reviews,omitempty"`
}

// Job is one band-hub job for the head, newest first.
type Job struct {
	ID      int64  `json:"id"`
	Queue   string `json:"queue"`
	Tests   string `json:"tests,omitempty"` // the job's tests param
	Verdict string `json:"verdict,omitempty"`
	State   string `json:"state,omitempty"`
	// Crates is a check-ni's crates checked, as the hub counted them (nil: unknown).
	Crates *int `json:"crates,omitempty"`
}

// Arch is the architect-review[bot] verdict comment, and its newest hold (a pass in progress).
type Arch struct {
	Verdict string `json:"verdict,omitempty"`
	Sha     string `json:"sha,omitempty"`
	Tree    string `json:"tree,omitempty"` // Sha's tree ("" not known)
	At      string `json:"at,omitempty"`
	Hold    bool   `json:"hold,omitempty"`
	HoldAt  string `json:"hold_at,omitempty"`
	HoldSha string `json:"hold_sha,omitempty"` // the head the newest hold names (its Head: line)
}

// Written is another bot's written verdict naming the head it read (the automaton's marker): it counts whatever
// state GitHub records for the bot.
type Written struct {
	By      string `json:"by"`
	Verdict string `json:"verdict"` // APPROVE, REQUEST_CHANGES, ...
	Sha     string `json:"sha"`
	Tree    string `json:"tree,omitempty"` // Sha's tree ("" not known)
}

// Marks are the owner-written marks the steps read (red_not_ours, rust_not_ours: the proof a red is main-side),
// looked up in Row first, then Item (the board row, then the board item).
type Marks struct {
	Row  map[string]any
	Item map[string]any
}

func (m Marks) str(k string) string {
	if s, ok := m.Row[k].(string); ok && s != "" {
		return s
	}
	s, _ := m.Item[k].(string)
	return s
}

// In is everything the checklist reads about one PR.
type In struct {
	Facts   Facts
	Jobs    []Job // band jobs at the head, newest first
	Marks   Marks
	Arch    *Arch
	Written []Written
	Files   []string // nil: not known
}

// Row is one evaluated checklist row.
type Row struct {
	ID     string `json:"id"`
	Label  string `json:"k"`
	OK     bool   `json:"ok"`
	Status string `json:"status"` // yes | no | pending | manual
	Note   string `json:"note"`
}

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}

func in(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// samePrefix says whether two hex ids name the same object: equal over the shorter, of at least 7.
func samePrefix(a, b string) bool {
	n := min(len(a), len(b))
	return n >= 7 && a[:n] == b[:n]
}

// AtHead says whether a verdict naming sha (whose tree is tree, "" unknown) stands at the head: the head itself, or a
// head with the same tree (rule.merge_ready: a same-tree head keeps it).
func AtHead(f *Facts, sha, tree string) (at, sameTree bool) {
	if samePrefix(sha, f.Oid) {
		return true, false
	}
	if samePrefix(tree, f.Tree) {
		return true, true
	}
	return false, false
}

var approving = []string{"APPROVE", "APPROVE_WITH_NOTES", "APPROVED"}

func label(v string) string { return strings.ToLower(strings.ReplaceAll(v, "_", " ")) }

// evBotApproval: ONE general bot approval at the head (or a same-tree head): the architect's verdict comment
// (APPROVE, APPROVE_WITH_NOTES), another bot's written verdict, or an APPROVED review by a Bots login. Approvals at
// other heads are shown, not counted.
func evBotApproval(c In) (bool, string, string) {
	f := &c.Facts
	var by, ev []string
	add := func(who, verdict, sha, tree string) {
		ev = append(ev, fmt.Sprintf("%s %s at %s", who, label(verdict), short(sha)))
		if !in(verdict, approving) {
			return
		}
		if at, same := AtHead(f, sha, tree); at && same {
			by = append(by, who+" (same tree)")
		} else if at {
			by = append(by, who)
		}
	}
	if a := c.Arch; a != nil && a.Verdict != "" {
		add("architect", a.Verdict, a.Sha, a.Tree)
	}
	for _, w := range c.Written {
		add(w.By, w.Verdict, w.Sha, w.Tree)
	}
	for _, r := range f.Reviews {
		if in(r.Login, Bots) {
			add(r.Login, r.State, r.Oid, r.Tree)
		}
	}
	seen := strings.Join(ev, "; ")
	if seen == "" {
		seen = "no bot verdict yet"
	}
	switch {
	case len(by) > 0:
		return true, "", "approved at the head by " + strings.Join(by, ", ") + "; " + seen
	case c.Arch != nil && c.Arch.Hold:
		return false, "pending", "architect pass in progress (hold posted " + c.Arch.HoldAt + "); " + seen
	}
	return false, "", "no bot approval at this head; " + seen
}

// evNoRed: checks race the merge, so pending or absent never blocks; only a red at the head does (a check-ni FAIL,
// a Runtime or Rust pipeline FAILURE or ERROR), until the board carries a not-ours proof (red_not_ours, or
// rust_not_ours for the Rust pipeline).
func evNoRed(c In) (bool, string, string) {
	proof := c.Marks.str("red_not_ours")
	rustProof := proof
	if rustProof == "" {
		rustProof = c.Marks.str("rust_not_ours")
	}
	var open, proven []string
	running := false
	red := func(what, pf string) {
		if pf == "" {
			open = append(open, what)
		} else {
			proven = append(proven, what+" (proven not ours: "+pf+")")
		}
	}
	if KindOf(c.Files) != "docs" {
		for _, j := range c.Jobs { // newest first: the latest check-ni
			if j.Queue != "aether.check-ni" {
				continue
			}
			running = j.Verdict == ""
			if j.Verdict != "" && j.Verdict != "PASS" {
				red(fmt.Sprintf("check-ni job %d %s", j.ID, j.Verdict), proof)
			}
			break
		}
	}
	for _, p := range []struct{ name, proof string }{{Runtime, proof}, {Rust, rustProof}} {
		isRed := false
		for _, x := range c.Facts.Contexts {
			if !strings.HasPrefix(x.Name, p.name) {
				continue
			}
			running = running || x.State == "PENDING"
			isRed = isRed || x.State == "FAILURE" || x.State == "ERROR"
		}
		if isRed {
			red(p.name+" red", p.proof)
		}
	}
	switch {
	case len(open) > 0:
		return false, "", "red at the head, ours until proven main-side: " + strings.Join(open, "; ")
	case len(proven) > 0:
		return true, "", strings.Join(proven, "; ")
	case running:
		return true, "", "no red yet; checks still running (not waited on)"
	}
	return true, "", "no red at the head"
}

// evConflict (Minister 11-11): only mergeable CONFLICTING or merge state DIRTY is a conflict. UNKNOWN is GitHub
// recomputing: never a wait; the last known value stands meanwhile. BLOCKED and BEHIND are not conflicts.
func evConflict(c In) (bool, string, string) {
	ms, mg := c.Facts.MergeState, c.Facts.Mergeable
	recomputing := mg == "" || mg == "UNKNOWN"
	if recomputing && c.Facts.LastMergeable != "" {
		mg = c.Facts.LastMergeable
	}
	conflict := mg == "CONFLICTING" || ms == "DIRTY"
	var note string
	switch {
	case recomputing && c.Facts.LastMergeable != "":
		note = "mergeable: recomputing (last known " + strings.ToLower(mg) + ")"
	case recomputing:
		note = "mergeable: recomputing"
	default:
		note = "mergeable " + strings.ToLower(mg)
	}
	if ms == "" {
		ms = "?"
	}
	note += ", merge state " + strings.ToLower(ms)
	if conflict {
		note += ": rebase on main"
	}
	return !conflict, "", note
}

var evaluators = map[string]func(In) (ok bool, status, note string){
	"bot-approval": evBotApproval, "no-red": evNoRed, "conflict": evConflict,
}

// Checklist is the ordered checklist for one PR: one row per step.
func Checklist(c In) []Row {
	out := make([]Row, 0, len(Steps))
	for _, s := range Steps {
		fn, ok := evaluators[s.ID]
		if !ok {
			out = append(out, Row{ID: s.ID, Label: s.Label, OK: true, Status: "manual", Note: "not automated: the Minister's ready.<pr>"})
			continue
		}
		yes, st, note := fn(c)
		if st == "" {
			st = map[bool]string{true: "yes", false: "no"}[yes]
		}
		out = append(out, Row{ID: s.ID, Label: s.Label, OK: yes, Status: st, Note: note})
	}
	return out
}

// StateOf is the PR's one state. steps nil means no checklist was computed.
func StateOf(f *Facts, steps []Row) string {
	switch {
	case f == nil:
		return "unknown"
	case f.State == "MERGED":
		return "merged"
	case f.State == "CLOSED":
		return "closed"
	case f.Draft:
		return "draft"
	case strings.ToUpper(f.Mergeable) == "CONFLICTING" || strings.ToUpper(f.MergeState) == "DIRTY":
		return "waiting: conflict" // light facts carry mergeability too
	case !f.Heavy || steps == nil:
		return "unknown"
	}
	// A conflict invalidates the head, so it overrides step order (board-sync 1e12bbbb 11-12). UNKNOWN never counts.
	for _, s := range steps {
		if s.ID == "conflict" && !s.OK {
			return "waiting: " + s.Label
		}
	}
	for _, s := range steps {
		if !s.OK {
			return "waiting: " + s.Label
		}
	}
	return "ready"
}

// ArchBlock is the architect's override of an open PR's state (Warden 11-13, narrowed by rule.merge_ready): a
// REQUEST_CHANGES verdict naming the head (or a same-tree head) blocks it ("blocked: architect RC at <h8>") until a
// later approving architect verdict replaces it (archOf keeps only the latest verdict comment) or an Ilya-Grebnov
// APPROVED review is submitted after it. An RC at an older head no longer blocks: the fix is pushed, and the
// bot-approval step asks for an approval at the new head. "" leaves the state as StateOf gave it.
func ArchBlock(f *Facts, a *Arch) string {
	if f == nil || a == nil || f.State != "OPEN" || f.Draft || a.Verdict != "REQUEST_CHANGES" {
		return ""
	}
	if at, _ := AtHead(f, a.Sha, a.Tree); !at || approvedAfter(f.Reviews, a.At) {
		return ""
	}
	return "blocked: architect RC at " + short(a.Sha)
}

// approvedAfter says whether an Ilya-Grebnov APPROVED review was submitted after at.
func approvedAfter(rs []Review, at string) bool {
	t, err := time.Parse(time.RFC3339, at)
	for _, r := range rs {
		if !strings.EqualFold(r.Login, Ilya) || r.State != "APPROVED" {
			continue
		}
		rt, rerr := time.Parse(time.RFC3339, r.At)
		if err == nil && rerr == nil && rt.After(t) {
			return true
		}
	}
	return false
}
