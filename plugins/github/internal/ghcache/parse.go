package ghcache

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The parsers: Go ports of the Minister's board-snapshot.py (architect,
// Praxis gate) and pr-body-evidence.py (reviewer rulings, human hold),
// plus the title, link, evidence and bot-marker readers DESIGN 3 names. Each
// reads gh's raw inputs only; a parse failure keeps the raw text in gh and
// names itself in Parsed.Errors (sync.errors), never drops the PR.

// Parsed is the entity form's parsed key.
type Parsed struct {
	Topics   []string  `json:"topics,omitempty"` // title tags that are not a stack position
	Stack    *Stack    `json:"stack,omitempty"`
	Type     string    `json:"type,omitempty"` // conventional title: type(scope): text
	Scope    string    `json:"scope,omitempty"`
	Links    Links     `json:"links"`
	Evidence *Evidence `json:"evidence,omitempty"`
	Agent    *Agent    `json:"agent,omitempty"`      // the last Agent: trailer of the body
	Written  *Agent    `json:"written_by,omitempty"` // Written by [figaro] <ver> (<model>)
	Created  string    `json:"created_by,omitempty"` // *Created by Praxis*
	Bots     Bots      `json:"bots"`
	Errors   []string  `json:"-"` // to sync.errors
}

// Stack is a stack position tag: [k-n], [k.j-n], [series k/n].
type Stack struct {
	Series string `json:"series,omitempty"`
	K      int    `json:"k"`
	Sub    int    `json:"sub,omitempty"`
	N      int    `json:"n"`
}

// Links are the body's issue references, sorted, deduplicated.
type Links struct {
	Fixes  []int `json:"fixes,omitempty"`
	PartOf []int `json:"part_of,omitempty"`
	Design []int `json:"design,omitempty"`
}

// Evidence is the body's evidence block (q.pr_evidence_block), first
// occurrence of each line, each at most EvidenceLine bytes.
type Evidence struct {
	Peer     string `json:"peer,omitempty"`
	Tests    string `json:"tests,omitempty"`
	CI       string `json:"ci,omitempty"`
	Premerge string `json:"premerge,omitempty"`
	Hold     string `json:"hold,omitempty"`
}

// EvidenceLine caps one evidence line on the form.
const EvidenceLine = 300

// Agent is a tool, its version and model.
type Agent struct {
	Tool  string `json:"tool"`
	Ver   string `json:"ver"`
	Model string `json:"model"`
}

// Bots are the bot and reviewer verdicts read by marker.
type Bots struct {
	Architect     []ArchVerdict   `json:"architect,omitempty"`
	ArchitectHold *Hold           `json:"architect_hold,omitempty"`
	Praxis        *Praxis         `json:"praxis,omitempty"`
	FIDO          *FIDO           `json:"fido,omitempty"`
	AutoEng       *AutoEng        `json:"autoeng,omitempty"`
	Automaton     *ReviewVerdict  `json:"automaton,omitempty"`
	Lukas         *ReviewVerdict  `json:"lukas,omitempty"`
	Whitten       *Whitten        `json:"whitten,omitempty"`
	Lair          map[string]Lair `json:"lair,omitempty"` // yugo, ken, mike
}

// ArchVerdict is one architect verdict (board-snapshot arch_verdicts).
type ArchVerdict struct {
	Verdict string `json:"verdict"` // approve | changes
	At      string `json:"at"`
	AtHead  bool   `json:"at_head"`
	Sha     string `json:"sha,omitempty"`
}

// Hold is the architect's review-in-progress comment newer than its last verdict.
type Hold struct {
	At   string `json:"at"`
	Head string `json:"head,omitempty"`
}

// Praxis is the newest Praxis merge gate comment (board-snapshot praxis_of).
type Praxis struct {
	Verdict string   `json:"verdict"` // not_safe | safe | unparsed
	Head    string   `json:"head"`
	AtHead  bool     `json:"at_head"`
	At      string   `json:"at"`
	URL     string   `json:"url"`
	Items   []string `json:"items"`
}

// FIDO is the newest review-verdict marker.
type FIDO struct {
	Verdict     string         `json:"verdict"`
	Head        string         `json:"head"`
	AtHead      bool           `json:"at_head"`
	BlocksMerge bool           `json:"blocks_merge"`
	Counts      map[string]int `json:"counts,omitempty"`
	At          string         `json:"at"`
}

// AutoEng is the newest AutoEng review.
type AutoEng struct {
	Verdict    string         `json:"verdict"`
	Confidence int            `json:"confidence"`
	Counts     map[string]int `json:"counts,omitempty"` // P0 P1 P2 NIT
	Sha        string         `json:"sha"`
	AtHead     bool           `json:"at_head"`
	State      string         `json:"state"` // the review's state (DISMISSED: history)
	At         string         `json:"at"`
}

// ReviewVerdict is a bot's newest decisive review (APPROVED,
// CHANGES_REQUESTED, DISMISSED).
type ReviewVerdict struct {
	State    string `json:"state"`
	Sha      string `json:"sha"`
	AtHead   bool   `json:"at_head"`
	At       string `json:"at"`
	Blocking int    `json:"blocking,omitempty"` // automaton: N Blocking findings
}

// Whitten is Christopher-Whitten's newest review verdict.
type Whitten struct {
	Verdict string `json:"verdict"`
	Tally   string `json:"tally,omitempty"`
	Sha     string `json:"sha"`
	AtHead  bool   `json:"at_head"`
	State   string `json:"state"`
	At      string `json:"at"`
}

// Lair is one of Akshay's reviewers' latest ruling (pr-body-evidence peer_line).
type Lair struct {
	Ruling string `json:"ruling"`
	Sha    string `json:"sha"`
	At     string `json:"at"`
	URL    string `json:"url"`
}

var (
	titleTags = regexp.MustCompile(`^\s*((?:\[[^\[\]]+\]\s*)+)(.*)$`)
	oneTag    = regexp.MustCompile(`\[([^\[\]]+)\]`)
	stackTag  = regexp.MustCompile(`^(?:(\S+)\s+)?(\d+)(?:\.(\d+))?[-/](\d+)$`)
	convTitle = regexp.MustCompile(`^([a-z]+)(?:\(([^)]+)\))?!?:\s`)
	linkRe    = regexp.MustCompile(`(?im)\b(fixes|closes|resolves|part of|design)\s+#(\d+)`)
	evidRe    = regexp.MustCompile(`(?m)^\s*(Peer|Tests|CI|Banana premerge|Human hold):\s*(.*)$`)
	writtenRe = regexp.MustCompile(`Written by \[(figaro)\]\([^)]*\) (\S+) \(([^)]+)\)`)
	agentRe   = regexp.MustCompile(`(?m)^Agent: ([\w.-]+)/(\S+) model=(\S+)\s*$`)
	createdRe = regexp.MustCompile(`(?m)^\*Created by (\w+)\*\s*$`)
)

// ParseTitle fills topics, stack, type and scope.
func ParseTitle(p *Parsed, title string) {
	rest := title
	if m := titleTags.FindStringSubmatch(title); m != nil {
		rest = m[2]
		for _, t := range oneTag.FindAllStringSubmatch(m[1], -1) {
			tag := strings.TrimSpace(t[1])
			if s := stackTag.FindStringSubmatch(tag); s != nil && p.Stack == nil {
				k, _ := strconv.Atoi(s[2])
				sub, _ := strconv.Atoi(s[3])
				n, _ := strconv.Atoi(s[4])
				p.Stack = &Stack{Series: s[1], K: k, Sub: sub, N: n}
				continue
			}
			p.Topics = append(p.Topics, tag)
		}
	}
	if m := convTitle.FindStringSubmatch(strings.TrimSpace(rest)); m != nil {
		p.Type, p.Scope = m[1], m[2]
	}
}

// ParseBody fills links, evidence, the agent trailer, written-by and created-by.
func ParseBody(p *Parsed, body string) {
	sets := map[string]map[int]bool{"fixes": {}, "part_of": {}, "design": {}}
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[2])
		switch strings.ToLower(m[1]) {
		case "fixes", "closes", "resolves":
			sets["fixes"][n] = true
		case "part of":
			sets["part_of"][n] = true
		case "design":
			sets["design"][n] = true
		}
	}
	p.Links = Links{Fixes: sortedInts(sets["fixes"]), PartOf: sortedInts(sets["part_of"]), Design: sortedInts(sets["design"])}
	var ev Evidence
	seen := false
	for _, m := range evidRe.FindAllStringSubmatch(body, -1) {
		v := cut(strings.TrimSpace(m[2]), EvidenceLine)
		dst := map[string]*string{"Peer": &ev.Peer, "Tests": &ev.Tests, "CI": &ev.CI, "Banana premerge": &ev.Premerge, "Human hold": &ev.Hold}[m[1]]
		if *dst == "" {
			*dst, seen = v, true
		}
	}
	if seen {
		p.Evidence = &ev
	}
	if ms := agentRe.FindAllStringSubmatch(body, -1); len(ms) > 0 {
		m := ms[len(ms)-1]
		p.Agent = &Agent{Tool: m[1], Ver: m[2], Model: m[3]}
	}
	if m := writtenRe.FindStringSubmatch(body); m != nil {
		p.Written = &Agent{Tool: m[1], Ver: m[2], Model: m[3]}
	}
	if m := createdRe.FindStringSubmatch(body); m != nil {
		p.Created = m[1]
	}
}

func sortedInts(m map[int]bool) []int {
	var out []int
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// cut is s at most n bytes, on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ArchitectLogins are the logins whose reviews are architect verdicts
// (filters.toml [approvers] logins), lower case.
var ArchitectLogins = map[string]bool{"architect-review": true, "ilya-grebnov": true}

const (
	archMark     = "<!-- architect-review -->"
	archHoldMark = "<!-- architect-review-hold -->"
)

var (
	archVerdict   = regexp.MustCompile(`\*\*(APPROVE_WITH_NOTES|APPROVE|REQUEST_CHANGES)\*\*`)
	archSha       = regexp.MustCompile("@ `([0-9a-f]{7,40})`")
	holdHead      = regexp.MustCompile(`Head: ([0-9a-f]{7,40})`)
	praxisHead    = regexp.MustCompile(`^## Praxis merge gate [\x{2014}-] PR #(\d+) @ ([0-9a-f]{7,40})`)
	praxisVerdict = regexp.MustCompile(`(?m)^\*\*(NOT SAFE|SAFE TO MERGE|SAFE)\*\*`)
	fidoMark      = regexp.MustCompile(`<!-- review-verdict (\{.*?\}) -->`)
	autoEngHead   = regexp.MustCompile(`^## AutoEng review: (.+?) \(confidence (\d+)%\)`)
	autoEngCounts = regexp.MustCompile(`P0: (\d+), P1: (\d+), P2: (\d+), NIT: (\d+)`)
	automatonBlk  = regexp.MustCompile(`(\d+) Blocking findings?\b`)
	whittenVerd   = regexp.MustCompile(`(?m)^(?:\*\*)?Verdict: ([A-Z_]+)`)
	whittenTally  = regexp.MustCompile(`(?m)^Tally:\s+(.+)$`)
	lairHead      = regexp.MustCompile(`(?is)^## (yugo|ken|mike)\b.*?(?:head|at) ([0-9a-f]{7,40})`)
	rulingWord    = regexp.MustCompile(`(?i)\b(APPROVE[DS]?|REQUEST[_ ]CHANGES|CHANGES[_ ]REQUESTED|BLOCKING)\b`)
	notBlocking   = regexp.MustCompile(`(?i)\b(?:not|non)[- ]blocking\b`)
	blockingWord  = regexp.MustCompile(`\bBLOCKING\b`)
	approvWord    = regexp.MustCompile(`(?i)\bapprov`)
	onBehalf      = regexp.MustCompile("(?i)submitted under `?[\\w-]+`?'s credential on the owner's behalf")
)

func atHead(sha, head string) bool { return sha != "" && strings.HasPrefix(head, sha) }

// ParseBots reads every bot verdict from comments and reviews.
func ParseBots(p *Parsed, pr *PR) {
	head := pr.Head
	tree := ""
	if len(pr.Commits.Nodes) > 0 && pr.Commits.Nodes[0].Commit.Tree != nil {
		tree = pr.Commits.Nodes[0].Commit.Tree.Oid
	}
	b := &p.Bots
	var holdAt, holdSha string
	for _, c := range pr.Comments.Nodes {
		body := c.Body
		if strings.Contains(body, archMark) {
			m, s := archVerdict.FindStringSubmatch(body), archSha.FindStringSubmatch(body)
			if m == nil {
				p.Errors = append(p.Errors, "unparseable: architect marker without a verdict: "+c.URL)
			} else {
				v := ArchVerdict{Verdict: "approve", At: c.CreatedAt}
				if m[1] == "REQUEST_CHANGES" {
					v.Verdict = "changes"
				}
				if s != nil {
					v.Sha, v.AtHead = s[1], atHead(s[1], head)
				}
				b.Architect = append(b.Architect, v)
			}
		} else if strings.Contains(body, archHoldMark) && c.CreatedAt > holdAt {
			holdAt, holdSha = c.CreatedAt, ""
			if h := holdHead.FindStringSubmatch(body); h != nil {
				holdSha = h[1]
			}
		}
		if m := praxisHead.FindStringSubmatch(body); m != nil {
			if n, _ := strconv.Atoi(m[1]); n == pr.Number && (b.Praxis == nil || c.CreatedAt > b.Praxis.At) {
				b.Praxis = praxis(body, m[2], head, c)
				if b.Praxis.Verdict == "unparsed" {
					p.Errors = append(p.Errors, "unparseable: praxis gate without a verdict: "+c.URL)
				}
			}
		}
		if f := fido(body, head, c.CreatedAt); f != nil && (b.FIDO == nil || f.At > b.FIDO.At) {
			b.FIDO = f
		}
		if m := lairHead.FindStringSubmatch(body); m != nil {
			who := strings.ToLower(m[1])
			if b.Lair == nil {
				b.Lair = map[string]Lair{}
			}
			if prev, ok := b.Lair[who]; !ok || c.CreatedAt >= prev.At {
				b.Lair[who] = Lair{Ruling: Ruling(body), Sha: m[2], At: c.CreatedAt, URL: c.URL}
			}
		}
	}
	for _, r := range pr.Reviews.Nodes {
		who := strings.ToLower(login(r.Author))
		sha, rtree := "", ""
		if r.Commit != nil {
			sha = r.Commit.Oid
			if r.Commit.Tree != nil {
				rtree = r.Commit.Tree.Oid
			}
		}
		if ArchitectLogins[who] && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			v := ArchVerdict{Verdict: "approve", At: r.SubmittedAt, AtHead: sha == head || (rtree != "" && rtree == tree), Sha: sha}
			if r.State == "CHANGES_REQUESTED" {
				v.Verdict = "changes"
			}
			b.Architect = append(b.Architect, v)
		}
		if f := fido(r.Body, head, r.SubmittedAt); f != nil && (b.FIDO == nil || f.At > b.FIDO.At) {
			b.FIDO = f
		}
		if m := autoEngHead.FindStringSubmatch(r.Body); m != nil && (b.AutoEng == nil || r.SubmittedAt > b.AutoEng.At) {
			conf, _ := strconv.Atoi(m[2])
			a := &AutoEng{Verdict: m[1], Confidence: conf, Sha: sha, AtHead: sha == head, State: r.State, At: r.SubmittedAt}
			if c := autoEngCounts.FindStringSubmatch(r.Body); c != nil {
				a.Counts = map[string]int{}
				for i, k := range []string{"P0", "P1", "P2", "NIT"} {
					a.Counts[k], _ = strconv.Atoi(c[i+1])
				}
			}
			b.AutoEng = a
		}
		decisive := r.State == "APPROVED" || r.State == "CHANGES_REQUESTED" || r.State == "DISMISSED"
		if who == "automaton" && decisive && (b.Automaton == nil || r.SubmittedAt > b.Automaton.At) {
			v := &ReviewVerdict{State: r.State, Sha: sha, AtHead: sha == head, At: r.SubmittedAt}
			if m := automatonBlk.FindStringSubmatch(r.Body); m != nil {
				v.Blocking, _ = strconv.Atoi(m[1])
			}
			b.Automaton = v
		}
		if who == "lukas" && decisive && onBehalf.MatchString(r.Body) && (b.Lukas == nil || r.SubmittedAt > b.Lukas.At) {
			b.Lukas = &ReviewVerdict{State: r.State, Sha: sha, AtHead: sha == head, At: r.SubmittedAt}
		}
		if who == "christopher-whitten" && (b.Whitten == nil || r.SubmittedAt > b.Whitten.At) {
			if m := whittenVerd.FindStringSubmatch(r.Body); m != nil {
				w := &Whitten{Verdict: m[1], Sha: sha, AtHead: sha == head, State: r.State, At: r.SubmittedAt}
				if t := whittenTally.FindStringSubmatch(r.Body); t != nil {
					w.Tally = cut(strings.TrimSpace(t[1]), 120)
				}
				b.Whitten = w
			}
		}
	}
	sort.SliceStable(b.Architect, func(i, j int) bool { return b.Architect[i].At < b.Architect[j].At })
	if holdAt != "" && (len(b.Architect) == 0 || holdAt > b.Architect[len(b.Architect)-1].At) {
		b.ArchitectHold = &Hold{At: holdAt, Head: holdSha}
	}
}

func praxis(body, sha, head string, c Comment) *Praxis {
	v := "unparsed"
	if m := praxisVerdict.FindStringSubmatch(body); m != nil {
		v = "safe"
		if m[1] == "NOT SAFE" {
			v = "not_safe"
		}
	}
	items := []string{}
	if i := strings.Index(body, "### Why NOT SAFE"); i >= 0 {
		lines := strings.Split(body[i:], "\n")[1:]
		for _, l := range lines {
			if strings.HasPrefix(l, "#") {
				break
			}
			if strings.HasPrefix(l, "- ") {
				items = append(items, cut(strings.TrimSpace(l[2:]), 240))
			}
		}
	}
	return &Praxis{Verdict: v, Head: cut(sha, 8), AtHead: strings.HasPrefix(head, sha), At: c.CreatedAt, URL: c.URL, Items: items}
}

func fido(body, head, at string) *FIDO {
	m := fidoMark.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var v struct {
		Head        string         `json:"head"`
		Verdict     string         `json:"verdict"`
		BlocksMerge bool           `json:"blocks_merge"`
		Counts      map[string]int `json:"counts"`
	}
	if json.Unmarshal([]byte(m[1]), &v) != nil || v.Verdict == "" {
		return &FIDO{Verdict: "unparsed", At: at}
	}
	return &FIDO{Verdict: v.Verdict, Head: cut(v.Head, 8), AtHead: v.Head != "" && v.Head == head, BlocksMerge: v.BlocksMerge, Counts: v.Counts, At: at}
}

// Ruling is a reviewer comment's verdict (pr-body-evidence ruling): the
// header's word first, else BLOCKING only for a BLOCKING that is not "not
// blocking" / "non-blocking", else approved / commented.
func Ruling(body string) string {
	head, _, _ := strings.Cut(body, "\n")
	if m := rulingWord.FindStringSubmatch(head); m != nil {
		if strings.HasPrefix(strings.ToUpper(m[1]), "APPROVE") {
			return "approved"
		}
		return "BLOCKING"
	}
	rest := notBlocking.ReplaceAllString(body, "")
	switch {
	case blockingWord.MatchString(rest):
		return "BLOCKING"
	case approvWord.MatchString(body):
		return "approved"
	case notBlocking.MatchString(body):
		return "ruled every item not BLOCKING"
	}
	return "commented"
}

// Parse runs every parser over one deep read.
func Parse(pr *PR) Parsed {
	var p Parsed
	ParseTitle(&p, pr.Title)
	ParseBody(&p, pr.Body)
	ParseBots(&p, pr)
	return p
}
