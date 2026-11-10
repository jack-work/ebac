// Package mergecheck is the PR merge checklist: one ordered row per step of the
// merge policy (RULE-chiqun-merge-criteria.md, sections "## <step-id>: <label>"),
// each evaluated from facts, and one state per PR (draft, "waiting: <label>" of
// the first unmet step, ready, merged, closed, unknown).
//
// It is a Go port of minister-control's states.py (dashboard-steward, e65964c),
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
)

// Job ids are band-hub job ids.
const (
	Ilya    = "Ilya-Grebnov" // the architect-review bot's APPROVE lands under this login
	Lukas   = "lukas"
	Runtime = "Aether Runtime PR Pipeline"
	Rust    = "Aether Rust PR Pipeline"
)

// FastJobs are the band-hub queues the band-jobs step waits on; TestJobs run
// tests, so a PASS with 0 tests is no verdict.
var (
	FastJobs = []string{"aether.check-ni", "aether.gate-kit", "aether.gate-ext", "aether.gate-services", "aether.clippy-pr"}
	TestJobs = []string{"aether.gate-kit", "aether.gate-ext", "aether.gate-services"}
)

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
}

// Facts are a PR's GitHub facts.
type Facts struct {
	Oid        string    `json:"oid"`
	State      string    `json:"state"` // OPEN, MERGED, CLOSED
	Draft      bool      `json:"draft"`
	Heavy      bool      `json:"heavy"` // full facts (checks, threads, reviews) were read
	Mergeable  string    `json:"mergeable,omitempty"`
	MergeState string    `json:"merge_state,omitempty"`
	Unresolved int       `json:"unresolved"`
	Contexts   []Context `json:"contexts,omitempty"`
	Reviews    []Review  `json:"reviews,omitempty"`
}

// Job is one band-hub job for the head, newest first.
type Job struct {
	ID      int64  `json:"id"`
	Queue   string `json:"queue"`
	Tests   string `json:"tests,omitempty"` // the job's tests param
	Verdict string `json:"verdict,omitempty"`
	State   string `json:"state,omitempty"`
}

// Arch is the architect-review[bot] verdict.
type Arch struct {
	Verdict string `json:"verdict,omitempty"`
	Sha     string `json:"sha,omitempty"`
	At      string `json:"at,omitempty"`
	Hold    bool   `json:"hold,omitempty"`
	HoldAt  string `json:"hold_at,omitempty"`
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
}

func evBandJobs(c ctx) res {
	type key struct{ q, t string }
	latest := map[key]Job{}
	var order []key
	for _, j := range c.jobs { // newest first: one verdict per (queue, tests param)
		if !in(j.Queue, FastJobs) {
			continue
		}
		k := key{j.Queue, j.Tests}
		if _, ok := latest[k]; !ok {
			latest[k] = j
			order = append(order, k)
		}
	}
	ok, pending := len(latest) > 0, false
	var ev []string
	for _, k := range order {
		j := latest[k]
		n, hasN := c.tests[j.ID]
		name := strings.SplitN(j.Queue, ".", 2)[1]
		if j.Tests != "" {
			name += "(" + j.Tests + ")"
		}
		// A test-running job passes only with a KNOWN, non-zero test count (the hub's /jobs/<id>/tests; until it
		// answers, the count is unknown and the job is pending, never a pass). states.py let an unknown count pass.
		passed := j.Verdict == "PASS" && !(in(j.Queue, TestJobs) && (!hasN || n == 0))
		if j.Verdict == "" || (j.Verdict == "PASS" && in(j.Queue, TestJobs) && !hasN) {
			pending = true
		}
		if !passed {
			ok = false
		}
		cnt := ""
		switch {
		case hasN:
			cnt = fmt.Sprintf(", %d tests", n)
		case in(j.Queue, TestJobs) && j.Verdict == "PASS":
			cnt = ", tests not read yet"
		}
		v := j.Verdict
		if v == "" {
			v = j.State
		}
		ev = append(ev, fmt.Sprintf("job %d %s: %s%s", j.ID, name, v, cnt))
	}
	note := strings.Join(ev, "; ")
	if note == "" {
		note = "no fast job on this head"
	}
	st := ""
	if !ok && pending {
		st = "pending"
	}
	return mk(ok, note, "", ev, st)
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

func evConflict(c ctx) res {
	// mergeStateStatus BLOCKED only means branch protection is unmet: later steps, not a conflict.
	ms, mg := c.f.MergeState, c.f.Mergeable
	ok := mg == "MERGEABLE" && ms != "DIRTY" && ms != "BEHIND"
	pend := mg == "UNKNOWN" || ms == "UNKNOWN"
	note := fmt.Sprintf("mergeable %s, merge state %s", lowerOr(mg), lowerOr(ms))
	if !ok && !pend {
		note += ": rebase on main"
	}
	st := ""
	if !ok && pend {
		st = "pending"
	}
	return mk(ok, note, "", nil, st)
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
	n := c.marks.get("chiqun_notified")
	if m, ok := n.(map[string]any); ok {
		at, chat, fu := m["at"], m["chat"], m["followups"]
		if at == nil {
			at = "?"
		}
		if chat == nil {
			chat = "?"
		}
		if fu == nil {
			fu = 0
		}
		head, _ := m["head"].(string)
		return mk(true, fmt.Sprintf("sent %v in %v; follow-ups %v", at, chat, num(fu)), head, nil, "")
	}
	if truthy(n) {
		return mk(true, fmt.Sprintf("sent: %v", n), "", nil, "")
	}
	return mk(false, "not yet", "", nil, "")
}

// num renders a JSON number the way Python prints an int.
func num(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
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
func Checklist(p Policy, f Facts, jobs []Job, marks Marks, tests map[int64]int, arch *Arch) []Row {
	c := ctx{f: f, jobs: jobs, marks: marks, tests: tests, head: f.Oid, arch: arch}
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
	case !f.Heavy || steps == nil:
		return "unknown"
	}
	for _, s := range steps {
		if !s.OK {
			return "waiting: " + s.Label
		}
	}
	return "ready"
}
