// Package mergecheck is the PR merge checklist: one ordered row per step of the
// merge policy (RULE-chiqun-merge-criteria.md, sections "## <step-id>: <label>"),
// each evaluated from facts, and one state per PR (draft, "waiting: <label>" of
// the first unmet step, ready, merged, closed, unknown).
//
// It is a Go port of minister-control's states.py (dashboard-steward, 846872c),
// which it replaces: ONE implementation (board-sync stage 1, 2026-11-10). The
// policy text is parsed on every call, so the checklist cannot drift from it:
// a policy step with no evaluator is "not automated" (never blocks, never
// passes silently); an evaluator whose step is gone is reported by SelfCheck.
// Pure: no I/O.
package mergecheck

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Job ids are band-hub job ids.
const (
	Ilya    = "Ilya-Grebnov" // the architect-review bot's APPROVE lands under this login
	Lukas   = "lukas"
	Runtime = "Aether Runtime PR Pipeline"
	Rust    = "Aether Rust PR Pipeline"
)

// FastJobs are the band-hub queues the band-jobs step reads; which of them
// are required is the policy's (RequiredJobs), the rest are opt-in evidence.
// TestJobs run tests (the hub's /jobs/<id>/tests counts them).
var (
	FastJobs = []string{"aether.check-ni", "aether.gate-kit", "aether.gate-ext", "aether.gate-services", "aether.clippy-pr"}
	TestJobs = []string{"aether.gate-kit", "aether.gate-ext", "aether.gate-services"}
)

var jobRE = regexp.MustCompile(`aether\.[a-z0-9-]+`)

// RequiredJobs are the band jobs the policy's band-jobs step requires at an
// open PR head, parsed from its text: every aether.<queue> named in a clause
// (split at ";" and ". ") that does not exempt it (a clause saying "not an
// open PR head", "never" or "opt-in" names jobs that are not required).
func RequiredJobs(p Policy) []string {
	var text string
	for _, s := range p.Steps {
		if s.ID == "band-jobs" {
			text = s.Text
		}
	}
	var out []string
	for _, sent := range strings.Split(text, ". ") {
		for _, clause := range strings.Split(sent, ";") {
			l := strings.ToLower(clause)
			if strings.Contains(l, "not an open pr head") || strings.Contains(l, "never") || strings.Contains(l, "opt-in") {
				continue
			}
			for _, q := range jobRE.FindAllString(clause, -1) {
				if !in(q, out) {
					out = append(out, q)
				}
			}
		}
	}
	return out
}

// KindOf is what a PR's changed files are (states.py _kind): docs when every
// file is a doc, tests when every file is a doc or a test, else code; ""
// when the files are not known (nil).
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

// Step is one policy section.
type Step struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Policy is the parsed policy file.
type Policy struct {
	Title string `json:"title"`
	Intro string `json:"intro"`
	Steps []Step `json:"steps"`
	Error string `json:"error,omitempty"`
}

var headRE = regexp.MustCompile(`^##\s+([a-z0-9-]+):\s*(.+?)\s*$`)

// Parse reads the policy text: the "# " title line, the first prose line, and
// one step per "## <id>: <label>" section in order; other "## " sections
// (WHEN, FOLLOW-UP) are policy only.
func Parse(text string) Policy {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var p Policy
	for _, l := range lines {
		if strings.HasPrefix(l, "# ") {
			p.Title = strings.TrimSpace(strings.TrimLeft(l, "# "))
			break
		}
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			p.Intro = strings.TrimSpace(l)
			break
		}
	}
	cur := -1
	for _, l := range lines {
		if m := headRE.FindStringSubmatch(l); m != nil {
			p.Steps = append(p.Steps, Step{ID: m[1], Label: m[2]})
			cur = len(p.Steps) - 1
			continue
		}
		if strings.HasPrefix(l, "## ") {
			cur = -1
			continue
		}
		if cur >= 0 && strings.TrimSpace(l) != "" {
			p.Steps[cur].Text = strings.TrimSpace(p.Steps[cur].Text + " " + strings.TrimSpace(l))
		}
	}
	if len(p.Steps) == 0 && text == "" {
		p.Error = "policy unreadable: no text"
	}
	return p
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
	At    string `json:"at,omitempty"` // submittedAt (RFC 3339, UTC)
}

// Facts are a PR's GitHub facts.
type Facts struct {
	Oid        string `json:"oid"`
	State      string `json:"state"` // OPEN, MERGED, CLOSED
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
	// Crates is a check-ni's crates checked, as the hub counted them (nil:
	// unknown, read as today). A PASS that checked 0 crates is no verdict.
	Crates *int `json:"crates,omitempty"`
}

// Arch is the architect-review[bot] verdict.
type Arch struct {
	Verdict string `json:"verdict,omitempty"`
	Sha     string `json:"sha,omitempty"`
	At      string `json:"at,omitempty"`
	Hold    bool   `json:"hold,omitempty"`
	HoldAt  string `json:"hold_at,omitempty"`
	HoldSha string `json:"hold_sha,omitempty"` // the head the newest hold names (its Head: line)
}

// Marks are the owner-written marks the steps read (majors_handled,
// majors_note, marginal, marginal_reason, chiqun_notified, rust_not_ours),
// looked up in Row first, then Item (the board row, then the board item).
type Marks struct {
	Row  map[string]any
	Item map[string]any
}

func (m Marks) get(k string) any {
	if v, ok := m.Row[k]; ok && v != nil {
		return v
	}
	if v, ok := m.Item[k]; ok {
		return v
	}
	return nil
}

func (m Marks) str(k string) string {
	if s, ok := m.Row[k].(string); ok && s != "" {
		return s
	}
	s, _ := m.Item[k].(string)
	return s
}

// boolOf is the mark as Python reads `is True` / `is False`: nil when unset.
func (m Marks) boolOf(k string) *bool {
	if b, ok := m.get(k).(bool); ok {
		return &b
	}
	return nil
}

// Row is one evaluated checklist row.
type Row struct {
	ID     string   `json:"id"`
	Label  string   `json:"k"`
	OK     bool     `json:"ok"`
	Status string   `json:"status"` // yes | no | pending | manual
	Note   string   `json:"note"`
	Ev     []string `json:"ev"`
	At     string   `json:"at"`
	Stale  bool     `json:"stale"`
}

type res struct {
	ok     bool
	status string
	note   string
	at     string
	ev     []string
	carry  bool
}

func mk(ok bool, note, at string, ev []string, status string) res {
	if status == "" {
		status = map[bool]string{true: "yes", false: "no"}[ok]
	}
	if ev == nil {
		ev = []string{}
	}
	return res{ok: ok, status: status, note: note, at: at, ev: ev}
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

type ctx struct {
	f     Facts
	jobs  []Job
	marks Marks
	tests map[int64]int
	head  string
	arch  *Arch
	files []string // nil: not known
	need  []string // the required band jobs (RequiredJobs)
}

func evBandJobs(c ctx) res {
	kind := KindOf(c.files)
	if kind == "docs" {
		return mk(true, "docs-only PR: no band job needed", "", nil, "")
	}
	type key struct{ q, t string }
	latest := map[key]Job{}
	var order []key
	for _, j := range c.jobs { // newest first: one verdict per (queue, tests param)
		if !in(j.Queue, FastJobs) && !in(j.Queue, c.need) {
			continue
		}
		k := key{j.Queue, j.Tests}
		if _, ok := latest[k]; !ok {
			latest[k] = j
			order = append(order, k)
		}
	}
	ok, pending := true, false
	var ev, names []string
	for _, q := range c.need {
		name := strings.SplitN(q, ".", 2)[1]
		names = append(names, name)
		var j *Job
		for _, k := range order {
			if k.q == q {
				jj := latest[k]
				j = &jj
				break
			}
		}
		if j == nil {
			ok = false
			ev = append(ev, name+": not run on this head")
			continue
		}
		if j.Verdict == "" {
			pending = true
		}
		if j.Verdict != "PASS" {
			ok = false
		}
		v := j.Verdict
		if v == "" {
			v = j.State
		}
		switch {
		case j.Verdict == "PASS" && q == "aether.check-ni" && j.Crates != nil && *j.Crates == 0:
			ok = false // the rule: a check-ni PASS with 0 crates checked is no verdict
			v += ", 0 crates checked: no verdict"
		case j.Crates != nil:
			v += fmt.Sprintf(", %d crates", *j.Crates)
		}
		ev = append(ev, fmt.Sprintf("job %d %s: %s", j.ID, name, v))
	}
	for _, k := range order { // opt-in targeted runs: evidence only, never a merge requirement
		if in(k.q, c.need) {
			continue
		}
		j := latest[k]
		t := ""
		if k.t != "" {
			t = "(" + k.t + ")"
		}
		v := j.Verdict
		if v == "" {
			v = j.State
		}
		line := fmt.Sprintf("opt-in job %d %s%s: %s", j.ID, strings.SplitN(k.q, ".", 2)[1], t, v)
		if n, has := c.tests[j.ID]; has {
			line += fmt.Sprintf(", %d tests", n)
		}
		ev = append(ev, line)
	}
	need := strings.Join(names, " + ")
	head := map[string]string{"tests": "tests-only PR: " + need + " only", "code": need + " at the head (clippy verdict = ADO Rust)",
		"": need + " at the head (files not read yet)"}[kind]
	st := ""
	if !ok && pending {
		st = "pending"
	}
	return mk(ok, head+"; "+strings.Join(ev, "; "), "", ev, st)
}

func ctxLine(x Context) string {
	s := x.Name + ": " + strings.ToLower(x.State)
	if x.Build != "" {
		s += " (build " + x.Build + ")"
	}
	return s
}

func evRuntimeCI(c ctx) res {
	var rt []Context
	for _, x := range c.f.Contexts {
		if strings.HasPrefix(x.Name, Runtime) {
			rt = append(rt, x)
		}
	}
	ok, pend := len(rt) > 0, false
	var ev []string
	for _, x := range rt {
		if x.State != "SUCCESS" {
			ok = false
		}
		if x.State == "PENDING" {
			pend = true
		}
		ev = append(ev, ctxLine(x))
	}
	note := strings.Join(ev, "; ")
	if note == "" {
		note = "no Runtime pipeline run at this head"
	}
	st := ""
	if !ok && pend {
		st = "pending"
	}
	return mk(ok, note, "", ev, st)
}

func evRustTriage(c ctx) res {
	var rust []Context
	for _, x := range c.f.Contexts {
		if strings.HasPrefix(x.Name, Rust) {
			rust = append(rust, x)
		}
	}
	st := "none"
	if len(rust) > 0 {
		green, red := true, false
		for _, x := range rust {
			if x.State != "SUCCESS" {
				green = false
			}
			if x.State == "FAILURE" || x.State == "ERROR" {
				red = true
			}
		}
		st = map[bool]string{true: "green", false: map[bool]string{true: "red", false: "running"}[red]}[green]
	}
	proof := c.marks.str("rust_not_ours")
	var ev []string
	for _, x := range rust {
		ev = append(ev, ctxLine(x))
	}
	if st == "red" {
		n := "not yet proven not ours"
		if proof != "" {
			n = "proven not ours: " + proof
		}
		return mk(proof != "", "Rust PR Pipeline red; "+n, "", ev, "")
	}
	return mk(true, map[string]string{"none": "no Rust pipeline run at this head (not waited on)", "green": "Rust PR Pipeline green",
		"running": "Rust PR Pipeline still running (not waited on)"}[st], "", ev, "")
}

func lowerOr(s string) string {
	if s == "" {
		return "?"
	}
	return strings.ToLower(s)
}

// evConflict (Minister 11-11): only mergeable CONFLICTING or merge state DIRTY
// is a conflict. UNKNOWN is GitHub recomputing: never a wait; the last known
// value stands meanwhile. BLOCKED (branch protection) and BEHIND are not
// conflicts.
func evConflict(c ctx) res {
	ms, mg := c.f.MergeState, c.f.Mergeable
	recomputing := mg == "" || mg == "UNKNOWN"
	if recomputing && c.f.LastMergeable != "" {
		mg = c.f.LastMergeable
	}
	conflict := mg == "CONFLICTING" || ms == "DIRTY"
	var note string
	switch {
	case recomputing && c.f.LastMergeable != "":
		note = "mergeable: recomputing (last known " + strings.ToLower(mg) + ")"
	case recomputing:
		note = "mergeable: recomputing"
	default:
		note = "mergeable " + strings.ToLower(mg)
	}
	note += ", merge state " + lowerOr(ms)
	if conflict {
		note += ": rebase on main"
	}
	return mk(!conflict, note, "", nil, "")
}

func evThreads(c ctx) res {
	n := c.f.Unresolved
	s := "s"
	if n == 1 {
		s = ""
	}
	return mk(n == 0, fmt.Sprintf("%d unresolved review thread%s", n, s), "", nil, "")
}

func review(c ctx, login string) *Review {
	for i := range c.f.Reviews {
		if c.f.Reviews[i].Login == login {
			return &c.f.Reviews[i]
		}
	}
	return nil
}

var approving = []string{"APPROVE", "APPROVE_WITH_NOTES"}

var verdictLabel = map[string]string{"APPROVE": "approve", "APPROVE_WITH_NOTES": "approve with notes", "REQUEST_CHANGES": "request changes"}

func archNote(a *Arch) string {
	if a == nil || a.Verdict == "" {
		return "no architect verdict yet"
	}
	l, ok := verdictLabel[a.Verdict]
	if !ok {
		l = strings.ToLower(strings.ReplaceAll(a.Verdict, "_", " "))
	}
	return fmt.Sprintf("architect %s at %s (%s)", l, short(a.Sha), a.At)
}

// majors: rule 5a. An APPROVE_WITH_NOTES counts only once every Major is handled (majors_handled + majors_note).
func majors(c ctx, a *Arch) (bool, string) {
	if a == nil || a.Verdict != "APPROVE_WITH_NOTES" {
		return true, ""
	}
	note := c.marks.str("majors_note")
	if h := c.marks.boolOf("majors_handled"); h != nil && *h {
		if note == "" {
			note = "yes"
		}
		return true, "Majors handled (Knight): " + note
	}
	return false, "Majors in the notes not yet marked handled (the Knight sets majors_handled + majors_note on the board)"
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

func archAtHead(a *Arch, head string) bool {
	return a != nil && in(a.Verdict, approving) && short(a.Sha) == short(head)
}

func evIlya(c ctx) res {
	a, r, head := c.arch, review(c, Ilya), c.head
	ilya := "no Ilya-Grebnov review (not required)"
	if r != nil {
		ilya = fmt.Sprintf("Ilya-Grebnov review: %s at %s", strings.ToLower(strings.ReplaceAll(r.State, "_", " ")), short(r.Oid))
	}
	sha := ""
	if a != nil {
		sha = a.Sha
	}
	if archAtHead(a, head) {
		ok, mnote := majors(c, a)
		return mk(ok, join(archNote(a), mnote, ilya), sha, nil, "")
	}
	if a != nil && a.Hold {
		return mk(false, fmt.Sprintf("architect pass in progress (hold posted %s); last %s; %s", a.HoldAt, archNote(a), ilya), sha, nil, "pending")
	}
	return mk(false, archNote(a)+"; "+ilya, sha, nil, "")
}

func evLukasCarryover(c ctx) res {
	a, lukas, head := c.arch, review(c, Lukas), c.head
	if archAtHead(a, head) {
		return mk(true, "not needed: the architect approved this head", "", nil, "")
	}
	if a == nil || !in(a.Verdict, approving) {
		return mk(false, "needs an approving architect verdict first (on any head)", "", nil, "")
	}
	mok, mnote := majors(c, a)
	marginal := c.marks.boolOf("marginal")
	reason := c.marks.str("marginal_reason")
	lk := lukas != nil && lukas.State == "APPROVED" && short(lukas.Oid) == short(head)
	mtxt := "interdiff not yet confirmed marginal (the Knight sets marginal on the board)"
	if marginal != nil {
		mtxt = map[bool]string{true: "interdiff marginal (board)", false: "interdiff NOT marginal (board): fresh architect pass needed"}[*marginal]
	}
	if reason != "" {
		reason = "Knight: " + reason
	}
	lt := "no lukas review"
	at := ""
	if lukas != nil {
		lt = fmt.Sprintf("lukas %s at %s", strings.ToLower(lukas.State), short(lukas.Oid))
		at = lukas.Oid
	}
	isMarginal := marginal != nil && *marginal
	r := mk(isMarginal && mok && lk, join(archNote(a)+" (an earlier head)", mnote, mtxt, reason, lt), at, nil, "")
	r.carry = isMarginal && mok
	return r
}

func evNotified(c ctx) res {
	ms, rd := strings.ToUpper(c.f.MergeState), strings.ToUpper(c.f.Review)
	ok := ms == "CLEAN" && rd == "APPROVED"
	rds := strings.ReplaceAll(strings.ToLower(rd), "_", " ")
	if rds == "" {
		rds = "none"
	}
	note := fmt.Sprintf("GitHub: merge state %s, review decision %s", lowerOr(ms), rds)
	if ok {
		note += ": mergeable now, the owner merges it (squash)"
	} else {
		note += ": not mergeable yet"
	}
	if m, isMap := c.marks.get("chiqun_notified").(map[string]any); isMap {
		at, chat := m["at"], m["chat"]
		if at == nil {
			at = "?"
		}
		if chat == nil {
			chat = "?"
		}
		note += fmt.Sprintf("; Chiqun FYI sent %v in %v", at, chat)
	}
	st := ""
	if !ok && (ms == "" || ms == "UNKNOWN") {
		st = "pending"
	}
	return mk(ok, note, "", nil, st)
}

var evaluators = map[string]func(ctx) res{
	"band-jobs": evBandJobs, "runtime-ci": evRuntimeCI, "rust-triage": evRustTriage, "conflict": evConflict,
	"threads": evThreads, "ilya": evIlya, "lukas-carryover": evLukasCarryover, "notified": evNotified,
}

// Automated says whether a policy step id has an evaluator.
func Automated(id string) bool { _, ok := evaluators[id]; return ok }

// Check is the drift report: evaluators with no policy step (loud), policy
// steps with no evaluator (shown as not automated).
type Check struct {
	OrphanEvaluators []string `json:"orphan_evaluators"`
	NotAutomated     []string `json:"not_automated"`
	Error            string   `json:"error"`
}

func SelfCheck(p Policy) Check {
	ids := map[string]bool{}
	c := Check{OrphanEvaluators: []string{}, NotAutomated: []string{}, Error: p.Error}
	for _, s := range p.Steps {
		ids[s.ID] = true
		if !Automated(s.ID) {
			c.NotAutomated = append(c.NotAutomated, s.ID)
		}
	}
	for id := range evaluators {
		if !ids[id] {
			c.OrphanEvaluators = append(c.OrphanEvaluators, id)
		}
	}
	sort.Strings(c.OrphanEvaluators)
	if c.Error == "" && len(p.Steps) == 0 {
		c.Error = "no steps parsed from the policy"
	}
	return c
}

// Checklist is the ordered checklist for one PR: one row per policy step.
// files are the PR's changed files (nil: not known yet).
func Checklist(p Policy, f Facts, jobs []Job, marks Marks, tests map[int64]int, arch *Arch, files []string) []Row {
	c := ctx{f: f, jobs: jobs, marks: marks, tests: tests, head: f.Oid, arch: arch, files: files, need: RequiredJobs(p)}
	results := map[string]res{}
	for id, fn := range evaluators {
		results[id] = fn(c)
	}
	if lc := results["lukas-carryover"]; lc.carry && !results["ilya"].ok { // 5b: the carry-over meets the architect step
		il := results["ilya"]
		il.ok, il.status, il.at = true, "yes", "" // an earlier head is expected here: not stale
		il.note += "; carried over (rule 5b, marginal interdiff)"
		results["ilya"] = il
	}
	out := make([]Row, 0, len(p.Steps))
	for _, s := range p.Steps {
		r, ok := results[s.ID]
		if !ok {
			out = append(out, Row{ID: s.ID, Label: s.Label, OK: true, Status: "manual", Note: "not automated: read the policy", Ev: []string{}})
			continue
		}
		at := short(c.head)
		if r.at != "" {
			at = short(r.at)
		}
		out = append(out, Row{ID: s.ID, Label: s.Label, OK: r.ok, Status: r.status, Note: r.note, Ev: r.ev, At: at,
			Stale: r.at != "" && c.head != "" && at != short(c.head)})
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
		return "waiting: conflict" // light facts carry mergeability too (states.py state_of)
	case !f.Heavy || steps == nil:
		return "unknown"
	}
	// A conflict invalidates the head, so it overrides step order (board-sync 1e12bbbb 11-12): "waiting: conflict"
	// even while an earlier step (band jobs, CI) is unmet. UNKNOWN never counts (evConflict: recomputing).
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

// ArchState is the architect's override of a PR's state (Warden 11-13, outside the states.py port, which the golden
// pins): the latest architect verdict REQUEST_CHANGES, not superseded by a later approving one, blocks the PR at once
// ("blocked: architect RC at <h8>", rc true). archOf keeps only the latest verdict comment, so a later APPROVE comment
// already replaces the RC; the bot's APPROVE can also land as an Ilya-Grebnov review, which supersedes the RC when
// submitted after it. Else a hold (a pass in progress, newer than the verdict) naming the PR's own head is
// "waiting: architect review". "" leaves the state as StateOf gave it.
func ArchState(f *Facts, a *Arch) (state string, rc bool) {
	if f == nil || a == nil || f.State != "OPEN" {
		return "", false
	}
	if a.Verdict == "REQUEST_CHANGES" && !approvedAfter(f.Reviews, a.At) {
		return "blocked: architect RC at " + short(a.Sha), true
	}
	if a.Hold && a.HoldSha != "" && short(a.HoldSha) == short(f.Oid) {
		return "waiting: architect review", false
	}
	return "", false
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
