package ghcache

import (
	"encoding/json"
	"strings"
	"testing"
)

// Title tags: one row per arm (topic, stack forms, conventional title, none) plus adversarial rows. RED if the stack
// regex loses a form, a stack tag leaks into topics, or a nested bracket reads as a tag.
func TestParseTitle(t *testing.T) {
	for _, c := range []struct {
		desc, title string
		want        string // JSON of {topics, stack, type, scope}
	}{
		{"topic and k-n stack", "[Effort ledger][24-29] feat(llm): x", `{"topics":["Effort ledger"],"stack":{"k":24,"n":29},"type":"feat","scope":"llm"}`},
		{"sub position k.j-n", "[A][3.2-7] fix: y", `{"topics":["A"],"stack":{"k":3,"sub":2,"n":7},"type":"fix"}`},
		{"series k/n", "[opt10 9/10] chore: z", `{"stack":{"series":"opt10","k":9,"n":10},"type":"chore"}`},
		{"only the first stack tag is the stack", "[1-2][3-4] x", `{"topics":["3-4"],"stack":{"k":1,"n":2}}`},
		{"no tags, conventional", "feat(effort-router): ask", `{"type":"feat","scope":"effort-router"}`},
		{"breaking bang", "feat(x)!: y", `{"type":"feat","scope":"x"}`},
		{"not conventional", "rust-crate-split: aether", `{}`},
		{"empty", "", `{}`},
		{"whitespace", "   ", `{}`},
		{"unicode tag", "[Ünïcode ✓] feat: x", `{"topics":["Ünïcode ✓"],"type":"feat"}`},
		{"nested brackets are no tag", "[a]b[c] feat: x", `{"topics":["a"]}`},
		{"tag mid-title is no tag", "feat: x [1-2]", `{"type":"feat"}`},
		{"unclosed bracket", "[abc feat: x", `{}`},
	} {
		var p Parsed
		ParseTitle(&p, c.title)
		got, _ := json.Marshal(struct {
			Topics []string `json:"topics,omitempty"`
			Stack  *Stack   `json:"stack,omitempty"`
			Type   string   `json:"type,omitempty"`
			Scope  string   `json:"scope,omitempty"`
		}{p.Topics, p.Stack, p.Type, p.Scope})
		if string(got) != c.want {
			t.Errorf("%s: %q -> %s, want %s", c.desc, c.title, got, c.want)
		}
	}
}

// Body links, evidence lines, trailers: both arms of every optional (present and absent), first-occurrence evidence,
// the last Agent trailer, the EvidenceLine cap (300 and 301 bytes), case-insensitive link verbs.
func TestParseBody(t *testing.T) {
	long := strings.Repeat("x", 301)
	for _, c := range []struct {
		desc, body, want string
	}{
		{"nothing", "", `{"links":{},"bots":{}}`},
		{"links of every verb, deduplicated and sorted", "Fixes #3\ncloses #1\nResolves #3\nPart of #9 / Design #8\nPART OF #9",
			`{"links":{"fixes":[1,3],"part_of":[9],"design":[8]},"bots":{}}`},
		{"a hash with no number is no link", "Fixes #abc", `{"links":{},"bots":{}}`},
		{"evidence: first occurrence of each line wins", "Peer: a\nTests: b\nCI: c\nBanana premerge: d\nHuman hold: e\nPeer: z",
			`{"links":{},"evidence":{"peer":"a","tests":"b","ci":"c","premerge":"d","hold":"e"},"bots":{}}`},
		{"evidence at the cap is kept whole", "Tests: " + long[:300], `{"links":{},"evidence":{"tests":"` + long[:300] + `"},"bots":{}}`},
		{"evidence one over the cap is cut to it", "Tests: " + long, `{"links":{},"evidence":{"tests":"` + long[:300] + `"},"bots":{}}`},
		{"the last Agent trailer wins", "Agent: figaro/0.1 model=a\n\nAgent: claude-code/2.1 model=b\n",
			`{"links":{},"agent":{"tool":"claude-code","ver":"2.1","model":"b"},"bots":{}}`},
		{"an Agent trailer mid-line is none", "see Agent: figaro/0.1 model=a", `{"links":{},"bots":{}}`},
		{"written by", "Written by [figaro](https://x/y) 0.71.1 (claude-opus-5.5) on behalf of J.",
			`{"links":{},"written_by":{"tool":"figaro","ver":"0.71.1","model":"claude-opus-5.5"},"bots":{}}`},
		{"created by", "*Created by Praxis*\n", `{"links":{},"created_by":"Praxis","bots":{}}`},
	} {
		var p Parsed
		ParseBody(&p, c.body)
		if got, _ := json.Marshal(p); string(got) != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.desc, got, c.want)
		}
	}
	// A multi-byte rune across the cap is cut before it, never split.
	var p Parsed
	ParseBody(&p, "Tests: "+strings.Repeat("a", 299)+"é")
	if p.Evidence.Tests != strings.Repeat("a", 299) {
		t.Errorf("rune split: %q", p.Evidence.Tests[290:])
	}
}

// Ruling: one row per outcome of its closed set (approved, BLOCKING, ruled every item not BLOCKING, commented),
// including the builder's two cases the Python test pins (Ken's APPROVE header; yugo's all-not-blocking).
func TestRuling(t *testing.T) {
	for _, c := range []struct{ desc, body, want string }{
		{"header approve", "## Ken peer review: APPROVE at abc\n\nBLOCKING nothing", "approved"},
		{"header request changes", "## yugo: REQUEST_CHANGES at abc", "BLOCKING"},
		{"body BLOCKING", "## yugo at abc\n\n1. BLOCKING: x", "BLOCKING"},
		{"not blocking only", "## yugo at abc\n\n1. not BLOCKING\n2. non-blocking", "ruled every item not BLOCKING"},
		{"approval word in body", "## yugo at abc\n\nI approve this.", "approved"},
		{"nothing", "## yugo at abc\n\nlooks fine", "commented"},
		{"empty", "", "commented"},
	} {
		if got := Ruling(c.body); got != c.want {
			t.Errorf("%s: %q, want %q", c.desc, got, c.want)
		}
	}
}

func comment(who, at, body string) Comment {
	return Comment{Author: &Login{who}, Body: body, CreatedAt: at, URL: "u-" + at}
}

func review(who, state, at, sha, body string) Review {
	r := Review{Author: &Login{who}, State: state, SubmittedAt: at, Body: body}
	r.Commit = &struct {
		Oid  string `json:"oid"`
		Tree *struct {
			Oid string `json:"oid"`
		} `json:"tree"`
	}{Oid: sha}
	return r
}

// Bot arms not in the fixture: a malformed marker becomes an error (raw kept), a FIDO marker with bad JSON reads
// unparsed, a hold newer than the last verdict is in progress, a praxis gate for another PR is ignored, lukas
// without the on-behalf line is no lukas verdict, a COMMENTED automaton review is not decisive.
func TestBotArms(t *testing.T) {
	pr := &PR{Number: 7, Head: "abcdef0123"}
	pr.Comments.Nodes = []Comment{
		comment("architect-review", "2026-01-01T00:00:00Z", "<!-- architect-review -->\n## Architect review @ `abcdef0` no verdict word"),
		comment("architect-review", "2026-01-02T00:00:00Z", "<!-- architect-review-hold -->\nHead: abcdef0123"),
		comment("Chiqun-Zhang", "2026-01-03T00:00:00Z", "## Praxis merge gate — PR #8 @ abcdef0\n\n**SAFE**"),
		comment("Chiqun-Zhang", "2026-01-04T00:00:00Z", "## Praxis merge gate - PR #7 @ abcdef0\n\nno verdict\n### Why NOT SAFE\n- one\n- two\n## next\n- three"),
		comment("dangau", "2026-01-05T00:00:00Z", `<!-- review-verdict {"v":1,broken} -->`),
	}
	pr.Reviews.Nodes = []Review{
		review("lukas", "CHANGES_REQUESTED", "2026-01-06T00:00:00Z", "abcdef0123", "a person's review"),
		review("automaton", "COMMENTED", "2026-01-07T00:00:00Z", "abcdef0123", "Early findings"),
		review("automaton", "CHANGES_REQUESTED", "2026-01-06T00:00:00Z", "0000", "> **⚠ 1 Blocking finding**"),
	}
	p := Parse(pr)
	got, _ := json.Marshal(struct {
		B Bots
		E []string
	}{p.Bots, p.Errors})
	want := `{"B":{"architect_hold":{"at":"2026-01-02T00:00:00Z","head":"abcdef0123"},` +
		`"praxis":{"verdict":"unparsed","head":"abcdef0","at_head":true,"at":"2026-01-04T00:00:00Z","url":"u-2026-01-04T00:00:00Z","items":["one","two"]},` +
		`"fido":{"verdict":"unparsed","head":"","at_head":false,"blocks_merge":false,"at":"2026-01-05T00:00:00Z"},` +
		`"automaton":{"state":"CHANGES_REQUESTED","sha":"0000","at_head":false,"at":"2026-01-06T00:00:00Z","blocking":1}},` +
		`"E":["unparseable: architect marker without a verdict: u-2026-01-01T00:00:00Z","unparseable: praxis gate without a verdict: u-2026-01-04T00:00:00Z"]}`
	if string(got) != want {
		t.Errorf("bots:\n got  %s\n want %s", got, want)
	}
}

// Checks: one row per state of the closed set, a job failure under a pipeline with no top run, run ids, status
// contexts, an unknown conclusion.
func TestChecksOf(t *testing.T) {
	cs := []Context{
		{Typename: "CheckRun", Name: "Aether Rust PR Pipeline", Conclusion: "FAILURE", DetailsURL: "https://x/_build/results?buildId=192089995"},
		{Typename: "CheckRun", Name: "Aether Rust PR Pipeline (Lint)", Conclusion: "FAILURE"},
		{Typename: "CheckRun", Name: "Aether Rust PR Pipeline (Build)", Conclusion: "SUCCESS"},
		{Typename: "CheckRun", Name: "Jobs only (A)", Conclusion: "TIMED_OUT"},
		{Typename: "CheckRun", Name: "Pending one", Status: "IN_PROGRESS"},
		{Typename: "CheckRun", Name: "Cancelled one", Conclusion: "CANCELLED"},
		{Typename: "CheckRun", Name: "Odd", Conclusion: "SOMETHING_NEW"},
		{Typename: "CheckRun", Name: "Unstarted (job)", Conclusion: "SUCCESS"},
		{Typename: "StatusContext", Context: "banana/premerge", State: "SUCCESS"},
	}
	got, _ := json.Marshal(ChecksOf(cs))
	want := `{"Aether Rust PR Pipeline":{"state":"fail","run_id":192089995,"jobs_failed":["Lint"]},"Cancelled one":{"state":"cancelled"},` +
		`"Jobs only":{"state":"fail","jobs_failed":["A"]},"Odd":{"state":"unknown"},"Pending one":{"state":"pending"},` +
		`"Unstarted":{"state":"pending"},"banana/premerge":{"state":"pass"}}`
	if string(got) != want {
		t.Errorf("checks:\n got  %s\n want %s", got, want)
	}
}

// Sections: preamble then one per H2, each hash + first 300 B; empty body has none; "### " is no split.
func TestSections(t *testing.T) {
	b := Sections("pre\n## One\nbody one\n### sub\n## Two\n" + strings.Repeat("y", 400))
	if len(b.Sections) != 3 || b.Sections[0].H != "" || b.Sections[0].Head != "pre" || b.Sections[1].H != "One" ||
		b.Sections[1].Head != "body one\n### sub" || b.Sections[2].H != "Two" || len(b.Sections[2].Head) != 300 {
		t.Errorf("sections: %+v", b.Sections)
	}
	if b.Bytes != 435 || len(b.Hash) != 12 {
		t.Errorf("bytes %d hash %q", b.Bytes, b.Hash)
	}
	if e := Sections(""); e.Bytes != 0 || e.Sections != nil {
		t.Errorf("empty: %+v", e)
	}
	if Sections("a\n## X\nb").Sections[1].Hash == Sections("a\n## X\nc").Sections[1].Hash {
		t.Error("an edited section kept its hash")
	}
}

// The newest Praxis gate by createdAt wins whatever order the comments come in (GitHub answers oldest first today;
// the guard does not rely on it). RED if the createdAt comparison goes (mutant P3: the later-listed, older gate wins).
func TestThePraxisGateNewestByTimeWins(t *testing.T) {
	pr := &PR{Number: 7, Head: "abcdef0123"}
	pr.Comments.Nodes = []Comment{
		comment("Chiqun-Zhang", "2026-01-05T00:00:00Z", "## Praxis merge gate — PR #7 @ abcdef0\n\n**SAFE TO MERGE**"),
		comment("Chiqun-Zhang", "2026-01-04T00:00:00Z", "## Praxis merge gate — PR #7 @ 1111111\n\n**NOT SAFE**"),
	}
	got := Parse(pr).Bots.Praxis
	if got == nil || got.Verdict != "safe" || got.At != "2026-01-05T00:00:00Z" || got.Head != "abcdef0" {
		t.Fatalf("praxis: %+v", got)
	}
}
