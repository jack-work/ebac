// Package boardsync is the pure half of board-sync reconciliation (plan B,
// ~/notes/decisions-minister/board-sync/PLAN-20261110.md): from our PRs' facts,
// their title tags and the Thinker's needs files it derives every lane
// (stack, index, front, needs) and rebuilds the board form's prs map. No I/O:
// the plugin hands it the board, the PR facts and the inputs, and writes what
// it returns.
//
// Rules:
//   - Stack, index and size come from the title tag [Stack][k-N] (N may be the
//     letter N: an unnumbered piece). A PR without a tag is parked in OTHER and
//     reported for judgement; it is never a front.
//   - The tag maps to a board stack by stacks.<KEY>.tag; a tag no stack declares
//     is learned from the stack the board already files its pieces under.
//   - Needs come from the stack's needs file (needs.<key lowercased> in the
//     inputs); a piece with no row needs the piece before it (k-1), so a stack
//     with no needs file is one lane.
//   - A front is a piece that is still open (draft or not) whose needs are all
//     no longer open (merged). Several fronts per stack are the Thinker's lanes.
//   - Codeword: <key lowercased>-<k>; untagged: other-<pr>.
//   - A merged or closed PR keeps its entry until its ledger record exists,
//     then the entry is archived (removed).
package boardsync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PR is one of our pull requests as the plugin knows it this pass.
type PR struct {
	Number    int
	Title     string // the GitHub title, tags included
	State     string // OPEN, MERGED, CLOSED
	Draft     bool
	Additions int
	Deletions int
	PState    string // the merge-checklist state (first unmet step), "" when not computed
}

func (p PR) open() bool { return p.State == "OPEN" }

var tagRE = regexp.MustCompile(`^\s*(\[[^\]\[]+\])\[(\d+)-(\d+|N)\]\s*(.*)$`)

// ParseTitle splits "[Stack][k-N] rest" into its tag, index and size (0 when
// N is the letter N) and the rest. ok is false for a title without the tag.
func ParseTitle(title string) (tag string, index, of int, rest string, ok bool) {
	m := tagRE.FindStringSubmatch(title)
	if m == nil {
		return "", 0, 0, strings.TrimSpace(title), false
	}
	index, _ = strconv.Atoi(m[2])
	if m[3] != "N" {
		of, _ = strconv.Atoi(m[3])
	}
	return m[1], index, of, strings.TrimSpace(m[4]), true
}

// Needs maps a PR number to the PR numbers it needs.
type Needs map[int][]int

// ParseNeeds reads one needs file: codeword<TAB>#pr<TAB>needs, needs a
// comma-separated list of PR numbers or "-"; # lines are comments. A bad line
// is reported, never guessed.
func ParseNeeds(tsv string) (Needs, []string) {
	out := Needs{}
	var bad []string
	sc := bufio.NewScanner(strings.NewReader(tsv))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			bad = append(bad, fmt.Sprintf("line %d: want 3 tab-separated fields, got %d: %q", n, len(f), line))
			continue
		}
		pr, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(f[1]), "#"))
		if err != nil {
			bad = append(bad, fmt.Sprintf("line %d: pr %q", n, f[1]))
			continue
		}
		out[pr] = []int{}
		if v := strings.TrimSpace(f[2]); v != "-" && v != "" {
			for _, x := range strings.Split(v, ",") {
				need, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(x), "#"))
				if err != nil {
					bad = append(bad, fmt.Sprintf("line %d: need %q", n, x))
					continue
				}
				out[pr] = append(out[pr], need)
			}
		}
	}
	return out, bad
}

// Input is one reconciliation.
type Input struct {
	Prs      map[string]json.RawMessage // the board's prs map, by codeword
	Stacks   map[string]json.RawMessage // the board's stacks map, by key
	PRs      []PR                       // our PRs this pass: every open one, and merged/closed ones still tracked
	Needs    map[string]string          // needs file text by stack key lowercased (inputs needs.<key>)
	Ledgered map[int]bool               // PRs whose ledger record exists
}

// Result is the rebuilt prs map and what changed.
type Result struct {
	Prs       map[string]json.RawMessage
	Added     []string
	Changed   []string
	Removed   []string
	Judgement []string // one line each, for the Minister
}

// Changed says whether the prs map must be written.
func (r Result) Dirty() bool { return len(r.Added)+len(r.Changed)+len(r.Removed) > 0 }

type piece struct {
	pr       PR
	stack    string // board stack key
	index    int
	of       int
	rest     string
	codeword string
	needs    []int
}

// Reconcile derives every lane and rebuilds the prs map.
func Reconcile(in Input) Result {
	var res Result
	old := map[int]string{} // pr -> codeword on the board
	oldEntry := map[int]map[string]json.RawMessage{}
	for cw, raw := range in.Prs {
		var e map[string]json.RawMessage
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		var n int
		if json.Unmarshal(e["pr"], &n) != nil || n == 0 {
			continue
		}
		old[n], oldEntry[n] = cw, e
	}

	// Tag -> stack key: declared tags first, then learned from the board's own pieces.
	tagKey := map[string]string{}
	for key, raw := range in.Stacks {
		var s struct {
			Tag string `json:"tag"`
		}
		if json.Unmarshal(raw, &s) == nil && s.Tag != "" {
			tagKey[s.Tag] = key
		}
	}
	for _, p := range in.PRs {
		tag, _, _, _, ok := ParseTitle(p.Title)
		if !ok || tagKey[tag] != "" {
			continue
		}
		if e := oldEntry[p.Number]; e != nil {
			var key string
			if json.Unmarshal(e["stack"], &key) == nil && key != "" && key != "OTHER" {
				tagKey[tag] = key // the board files its pieces under a stack it may not declare (CACHE)
			}
		}
	}

	byPR := map[int]*piece{}
	var pieces []*piece
	unknownTags := map[string][]int{}
	for _, p := range in.PRs {
		tag, k, n, rest, ok := ParseTitle(p.Title)
		pc := &piece{pr: p, rest: rest, index: k, of: n}
		switch key := tagKey[tag]; {
		case !ok:
			pc.stack, pc.index, pc.of, pc.codeword = "OTHER", 0, 0, fmt.Sprintf("other-%d", p.Number)
			if p.open() {
				res.Judgement = append(res.Judgement, fmt.Sprintf("#%d has no stack tag: parked in OTHER, never a front (%.60s)", p.Number, p.Title))
			}
		case key == "":
			pc.stack, pc.codeword = "OTHER", fmt.Sprintf("other-%d", p.Number)
			unknownTags[tag] = append(unknownTags[tag], p.Number)
		default:
			pc.stack, pc.codeword = key, fmt.Sprintf("%s-%d", strings.ToLower(key), k)
		}
		pieces = append(pieces, pc)
		byPR[p.Number] = pc
	}
	for _, tag := range sortedKeys(unknownTags) {
		res.Judgement = append(res.Judgement, fmt.Sprintf("tag %s names no board stack (add stacks.<KEY>.tag): %s parked in OTHER", tag, prList(unknownTags[tag])))
	}

	// Codeword collisions (two PRs claim [k] of one stack) are judgement, never a silent overwrite.
	seen := map[string]*piece{}
	for _, pc := range pieces {
		if prev := seen[pc.codeword]; prev != nil {
			res.Judgement = append(res.Judgement, fmt.Sprintf("%s claimed by #%d and #%d: the later is parked as other-%d", pc.codeword, prev.pr.Number, pc.pr.Number, pc.pr.Number))
			pc.stack, pc.codeword = "OTHER", fmt.Sprintf("other-%d", pc.pr.Number)
		}
		seen[pc.codeword] = pc
	}

	// Needs: the stack's file, else the piece before it.
	files := map[string]Needs{}
	for key, text := range in.Needs {
		n, bad := ParseNeeds(text)
		files[strings.ToLower(key)] = n
		for _, b := range bad {
			res.Judgement = append(res.Judgement, fmt.Sprintf("needs.%s %s", key, b))
		}
	}
	byStackIndex := map[string]map[int]*piece{}
	maxOf := map[string]int{}
	for _, pc := range pieces {
		if pc.stack == "OTHER" {
			continue
		}
		if byStackIndex[pc.stack] == nil {
			byStackIndex[pc.stack] = map[int]*piece{}
		}
		byStackIndex[pc.stack][pc.index] = pc
		if pc.of > maxOf[pc.stack] {
			maxOf[pc.stack] = pc.of
		}
	}
	for _, pc := range pieces {
		if pc.stack == "OTHER" {
			continue
		}
		if f, ok := files[strings.ToLower(pc.stack)]; ok {
			if need, ok := f[pc.pr.Number]; ok {
				pc.needs = need
				continue
			}
		}
		if pc.index >= 2 {
			if prev := byStackIndex[pc.stack][pc.index-1]; prev != nil {
				pc.needs = []int{prev.pr.Number}
			}
		}
	}

	open := func(n int) bool {
		if pc := byPR[n]; pc != nil {
			return pc.pr.open()
		}
		return false // not ours this pass: merged or closed long ago
	}
	codewordOf := func(n int) string {
		if pc := byPR[n]; pc != nil {
			return pc.codeword
		}
		return fmt.Sprintf("#%d", n)
	}

	res.Prs = map[string]json.RawMessage{}
	for _, pc := range pieces {
		p := pc.pr
		if !p.open() && in.Ledgered[p.Number] {
			continue // archived: its ledger record carries it now
		}
		front := false
		if p.open() && pc.stack != "OTHER" {
			front = true
			for _, n := range pc.needs {
				if open(n) {
					front = false
				}
			}
		}
		for _, n := range pc.needs {
			if pcn := byPR[n]; pcn != nil && pcn.pr.State == "CLOSED" && p.open() {
				res.Judgement = append(res.Judgement, fmt.Sprintf("%s (#%d) needs #%d, which was closed without merging", pc.codeword, p.Number, n))
			}
		}
		e := map[string]json.RawMessage{}
		for k, v := range oldEntry[p.Number] { // keep what the board's owners added (owner, ...)
			e[k] = v
		}
		set := func(k string, v any) { e[k] = mustJSON(v) }
		var needs []string
		for _, n := range pc.needs {
			needs = append(needs, codewordOf(n))
		}
		of := pc.of
		if of == 0 {
			of = maxOf[pc.stack]
		}
		set("pr", p.Number)
		set("stack", pc.stack)
		set("index", pc.index)
		set("of", of)
		set("front", front)
		set("needs", strings.Join(needs, ","))
		set("title", pc.rest)
		set("state", state(p))
		if p.PState != "" {
			set("pstate", p.PState)
		}
		if p.Additions+p.Deletions > 0 {
			l := fmt.Sprintf("+%d", p.Additions)
			if p.Deletions > 0 {
				l += fmt.Sprintf("/-%d", p.Deletions)
			}
			set("lines", l)
		}
		raw := mustJSON(e)
		res.Prs[pc.codeword] = raw
		switch prev, had := old[p.Number]; {
		case !had:
			res.Added = append(res.Added, pc.codeword)
		case prev != pc.codeword || !jsonEqual(in.Prs[prev], raw):
			res.Changed = append(res.Changed, pc.codeword)
		}
	}
	for cw := range in.Prs {
		if _, ok := res.Prs[cw]; !ok {
			res.Removed = append(res.Removed, cw)
		}
	}
	// A board entry for a PR we no longer see at all (not open, not tracked): removed above; say so when it was
	// not ledgered, since its record may be missing.
	for n, cw := range old {
		if byPR[n] == nil {
			res.Judgement = append(res.Judgement, fmt.Sprintf("%s (#%d) is on the board but is neither open nor tracked: removed, check its ledger record", cw, n))
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Changed)
	sort.Strings(res.Removed)
	sort.Strings(res.Judgement)
	return res
}

func state(p PR) string {
	switch {
	case p.State == "MERGED":
		return "merged"
	case p.State == "CLOSED":
		return "closed"
	case p.Draft:
		return "draft"
	}
	return "open"
}

// Fronts lists every front codeword by stack, for summaries and tests.
func Fronts(prs map[string]json.RawMessage) map[string][]string {
	out := map[string][]string{}
	for cw, raw := range prs {
		var e struct {
			Front bool   `json:"front"`
			Stack string `json:"stack"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Front {
			out[e.Stack] = append(out[e.Stack], cw)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func prList(ns []int) string {
	sort.Ints(ns)
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(s, ",")
}

func sortedKeys(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// jsonEqual compares two JSON values whatever their layout.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	return string(mustJSON(x)) == string(mustJSON(y))
}
