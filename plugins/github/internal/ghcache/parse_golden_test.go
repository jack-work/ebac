package ghcache

import (
	_ "embed"
	"encoding/json"
	"reflect"

	"strconv"
	"testing"
)

// test-data: testdata/deep-6.json (a deep read of six bic/aether PRs, 2026-11-20; comment and review bodies cut at
// 3000 B and threads at 12 in the fixture only), testdata/golden-ref.json (gen-goldens.py: the Python reference over it).

//go:embed testdata/deep-6.json
var deep6 []byte

//go:embed testdata/golden-ref.json
var goldenRef []byte

func fixturePRs(t *testing.T) map[int]*PR {
	t.Helper()
	var d struct {
		Data struct {
			Repository map[string]*PR `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(deep6, &d); err != nil {
		t.Fatal(err)
	}
	out := map[int]*PR{}
	for _, pr := range d.Data.Repository {
		out[pr.Number] = pr
	}
	if len(out) != 6 {
		t.Fatalf("fixture holds %d PRs, want 6", len(out))
	}
	return out
}

// The architect verdicts, the Praxis gate and the lair rulings equal the Python reference's (board-snapshot
// arch_verdicts / praxis_of, pr-body-evidence ruling) on every fixture PR. RED if a marker, regex or the at_head rule
// drifts from the reference (mutants P1-P4 in MUTANTS.md).
func TestBotsEqualThePythonReference(t *testing.T) {
	var ref map[string]struct {
		Architect []ArchVerdict `json:"architect"`
		Praxis    *Praxis       `json:"praxis"`
		Lair      map[string]struct {
			Ruling, Sha, URL string
		} `json:"lair"`
	}
	if err := json.Unmarshal(goldenRef, &ref); err != nil {
		t.Fatal(err)
	}
	prs := fixturePRs(t)
	if len(ref) != len(prs) {
		t.Fatalf("reference covers %d PRs, fixture %d", len(ref), len(prs))
	}
	arch, prax, lair := 0, 0, 0
	for ns, want := range ref {
		n, _ := strconv.Atoi(ns)
		got := Parse(prs[n]).Bots
		if !reflect.DeepEqual(got.Architect, want.Architect) && !(len(got.Architect) == 0 && len(want.Architect) == 0) {
			t.Errorf("#%d architect:\n got  %+v\n want %+v", n, got.Architect, want.Architect)
		}
		if !reflect.DeepEqual(got.Praxis, want.Praxis) {
			t.Errorf("#%d praxis:\n got  %+v\n want %+v", n, got.Praxis, want.Praxis)
		}
		if len(got.Lair) != len(want.Lair) {
			t.Errorf("#%d lair: got %v, want %v", n, got.Lair, want.Lair)
		}
		for who, w := range want.Lair {
			if g := got.Lair[who]; g.Ruling != w.Ruling || g.Sha != w.Sha || g.URL != w.URL {
				t.Errorf("#%d lair %s: got %+v, want %+v", n, who, g, w)
			}
		}
		arch += len(want.Architect)
		lair += len(want.Lair)
		if want.Praxis != nil {
			prax++
		}
	}
	// The pin must fail if its subject disappears: the reference holds 20 architect verdicts, 1 gate, 2 rulings.
	if arch != 20 || prax != 1 || lair != 2 {
		t.Fatalf("reference census: %d architect, %d praxis, %d lair; want 20, 1, 2", arch, prax, lair)
	}
}

// Every other parser's output on the fixture, written down literally from the bodies (checked by hand, 2026-11-20).
func TestParsedGoldens(t *testing.T) {
	prs := fixturePRs(t)
	for _, c := range []struct {
		n    int
		want func(p Parsed) string // "" when the parse is as written
	}{
		{116018, func(p Parsed) string {
			return diff(
				eq("topics", p.Topics, []string{"Effort ledger"}),
				eq("stack", p.Stack, &Stack{Series: "opt10", K: 9, N: 10}),
				eq("type/scope", p.Type+"/"+p.Scope, "feat/runtime"),
				eq("links", p.Links, Links{Fixes: []int{127551}, PartOf: []int{107264}, Design: []int{107997}}),
				eq("evidence.premerge", p.Evidence.Premerge, "not run at 56be9b24."),
				eq("evidence.peer prefix", p.Evidence.Peer[:47], "architect review approved at 56be9b24; Ken peer"),
				eq("evidence.hold prefix", p.Evidence.Hold[:6], "none. "),
				eq("agent", p.Agent, &Agent{Tool: "figaro", Ver: "0.59.0", Model: "claude-opus-5.5"}),
				eq("written", p.Written, &Agent{Tool: "figaro", Ver: "0.71.1", Model: "claude-opus-5.5"}),
				eq("created", p.Created, ""),
				eq("fido", p.Bots.FIDO, &FIDO{Verdict: "request_changes", Head: "56be9b24", AtHead: true, BlocksMerge: true,
					Counts: map[string]int{"blocking": 1, "not_blocking": 10}, At: "2026-11-19T02:06:34Z"}),
				eq("autoeng", p.Bots.AutoEng.Verdict+"/"+strconv.Itoa(p.Bots.AutoEng.Confidence)+"/"+p.Bots.AutoEng.At, "Approve/65/2026-11-19T01:37:21Z"),
				eq("autoeng counts", p.Bots.AutoEng.Counts, map[string]int{"P0": 0, "P1": 0, "P2": 3, "NIT": 0}),
				eq("automaton", p.Bots.Automaton.State, "DISMISSED"),
				eq("lukas", p.Bots.Lukas.State, "DISMISSED"),
				eq("whitten", p.Bots.Whitten.Verdict, "DISMISSED_OWN_RC_COMMENT"),
				eq("errors", p.Errors, []string(nil)),
			)
		}},
		{131611, func(p Parsed) string {
			return diff(
				eq("topics", p.Topics, []string{"Security Judge MD1"}),
				eq("stack", p.Stack, &Stack{K: 5, N: 10}),
				eq("type/scope", p.Type+"/"+p.Scope, "feat/security-judge"),
				eq("links", p.Links, Links{Fixes: []int{130984}, PartOf: []int{130874}}),
				eq("evidence", p.Evidence, (*Evidence)(nil)),
				eq("agent", p.Agent, &Agent{Tool: "claude-code", Ver: "2.1.328", Model: "claude-opus-5-5"}),
				eq("written", p.Written, (*Agent)(nil)),
				eq("created", p.Created, "Praxis"),
				eq("automaton", p.Bots.Automaton.State+"/"+strconv.FormatBool(p.Bots.Automaton.AtHead), "CHANGES_REQUESTED/false"),
				eq("whitten", p.Bots.Whitten.Verdict, "COMMENT"),
				eq("whitten tally", p.Bots.Whitten.Tally, "0 blocker · 0 important · 4 minor · 2 nit (claude 6, copilot 0, both 0)"),
				eq("fido", p.Bots.FIDO, &FIDO{Verdict: "approve_with_suggestions", Head: "39b37d84", AtHead: false,
					Counts: map[string]int{"blocking": 0, "not_blocking": 2}, At: "2026-11-19T08:02:05Z"}),
			)
		}},
		{130950, func(p Parsed) string {
			return diff(
				eq("topics", p.Topics, []string(nil)),
				eq("stack", p.Stack, (*Stack)(nil)),
				eq("type/scope", p.Type+"/"+p.Scope, "feat/effort-router"),
				eq("whitten", p.Bots.Whitten.Verdict, "REQUEST_CHANGES"),
			)
		}},
		{127740, func(p Parsed) string {
			return diff(
				eq("topics", p.Topics, []string{"Effort codeowners"}),
				eq("stack", p.Stack, &Stack{K: 0, N: 1}),
				eq("links", p.Links, Links{Fixes: []int{127666}}),
				eq("lair", len(p.Bots.Lair), 0), // "Peer review (yugo) at head" is not the ## header form
			)
		}},
		{107698, func(p Parsed) string {
			return diff(
				eq("topics", p.Topics, []string(nil)),
				eq("type", p.Type, ""), // "rust-crate-split: ..." is not type(scope):
				eq("agent", p.Agent, &Agent{Tool: "factory", Ver: "0.1.0", Model: "claude-opus-5.5"}),
				eq("autoeng", p.Bots.AutoEng.Verdict+"/"+p.Bots.AutoEng.State, "Request changes/CHANGES_REQUESTED"),
				eq("fido", p.Bots.FIDO.Verdict+"/"+strconv.FormatBool(p.Bots.FIDO.BlocksMerge), "approve_with_suggestions/false"),
				eq("hold", p.Bots.ArchitectHold, (*Hold)(nil)), // the verdicts are newer than the hold
			)
		}},
	} {
		if d := c.want(Parse(prs[c.n])); d != "" {
			t.Errorf("#%d:%s", c.n, d)
		}
	}
}

func eq(name string, got, want any) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return "\n  " + name + ": got " + string(g) + ", want " + string(w)
}

func diff(parts ...string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}
