package praxis

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// test-data: an invented example tree (issue 0 #1000, piece #1001, a deeper piece under it);
// goldens in testdata/*.golden (go test -update rewrites them).

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update once)", p, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func parentIssue() Issue {
	return Issue{
		Number: 1000, Topic: "Example topic", K: 0, N: 1,
		Summary:  "What the whole stack delivers",
		Delivers: "the outcome the stack exists for",
		Status:   Split, Assignee: "ours",
		Children: []Child{{Number: 1001, K: 1, Summary: "First piece", Status: InProgress}},
	}
}

func childIssue() Issue {
	return Issue{
		Number: 1001, Topic: "Example topic", K: 1, N: 1, Parent: 1000,
		Summary:  "First piece",
		Delivers: "one checkable outcome of this piece",
		Status:   InProgress, Assignee: "ours",
		Contract: "Delivers: one checkable outcome of this piece\nOwner: ours\nStatus: building",
	}
}

var openParent = &ParentState{Number: 1000, Open: true, Status: Split}

func TestRenderGoldens(t *testing.T) {
	p, c := parentIssue(), childIssue()
	deeper := c
	deeper.Number, deeper.Parent, deeper.K, deeper.N, deeper.Topic = 0, 1001, 2, 3, "Example topic"
	deeper.Summary, deeper.Status, deeper.Contract = "A deeper piece", Claimed, ""
	done := c
	done.Status, done.PRs = Done, []int{2001, 2002}
	cases := map[string]string{
		"parent.title": Title(p),
		"parent.body":  Body(p),
		"parent.split": SplitComment(p),
		"child.title":  Title(c),
		"child.body":   Body(c),
		"child.claim":  ClaimComment(c),
		"child.status": StatusComment(c),
		"child.done":   DoneComment(done),
		"deeper.title": Title(deeper),
		"deeper.body":  Body(deeper),
	}
	for name, got := range cases {
		t.Run(name, func(t *testing.T) { golden(t, name, got) })
	}
}

// Every refusal: the form, the parent state and the rule it must raise. RED with the validator's
// matching check removed (each row names one rule; mutants M1-M13 delete one check each).
func TestValidateRefusals(t *testing.T) {
	c := childIssue()
	mut := func(f func(*Issue)) Issue { i := c; f(&i); return i }
	cases := []struct {
		name   string
		issue  Issue
		parent *ParentState
		rule   Rule
	}{
		{"empty topic", mut(func(i *Issue) { i.Topic = " " }), openParent, RuleBadTopic},
		{"bracket in topic", mut(func(i *Issue) { i.Topic = "a]b" }), openParent, RuleBadTopic},
		{"n zero", mut(func(i *Issue) { i.N = 0 }), openParent, RuleNOutOfRange},
		{"k above n", mut(func(i *Issue) { i.K = 2 }), openParent, RuleKOutOfRange},
		{"k negative", mut(func(i *Issue) { i.K = -1 }), openParent, RuleKOutOfRange},
		{"child without parent", mut(func(i *Issue) { i.Parent = 0 }), nil, RuleNoParent},
		{"own parent", mut(func(i *Issue) { i.Parent = i.Number }), openParent, RuleSelfParent},
		{"closed parent", c, &ParentState{Number: 1000, Open: false, Status: Split}, RuleParentClosed},
		{"done parent", c, &ParentState{Number: 1000, Open: true, Status: Done}, RuleParentDone},
		{"empty summary", mut(func(i *Issue) { i.Summary = "" }), openParent, RuleEmptySummary},
		{"empty delivers", mut(func(i *Issue) { i.Delivers = "  " }), openParent, RuleEmptyDelivers},
		{"bad status", mut(func(i *Issue) { i.Status = "started" }), openParent, RuleBadStatus},
		{"split without children", mut(func(i *Issue) { i.Status = Split }), openParent, RuleSplitWithoutChildren},
		{"child k above parent n", func() Issue { p := parentIssue(); p.Children[0].K = 2; return p }(), nil, RuleChildOutOfRange},
		{"done without prs", mut(func(i *Issue) { i.Status = Done }), openParent, RuleDoneWithoutPRs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.issue, tc.parent)
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("want refusal %s, got %v", tc.rule, err)
			}
			if r.Rule != tc.rule {
				t.Fatalf("want %s, got %s (%s)", tc.rule, r.Rule, r.Detail)
			}
			if r.AlarmKey() != "sync.alarm.praxis."+string(tc.rule) {
				t.Fatalf("alarm key %q", r.AlarmKey())
			}
		})
	}
}

// The accepted shapes: a parent, a child under an open parent, a done child with PRs, a done
// parent whose children are all done (no PRs of its own), a deeper-tier issue 0 under a child.
func TestValidateAccepts(t *testing.T) {
	p, c := parentIssue(), childIssue()
	done := c
	done.Status, done.PRs = Done, []int{2001}
	pdone := p
	pdone.Status = Done
	pdone.Children = []Child{{Number: 1001, K: 1, Summary: "x", Status: Done}}
	tier := p
	tier.Number, tier.Parent = 1100, 1001
	for name, tc := range map[string]struct {
		i Issue
		p *ParentState
	}{
		"parent":                {p, nil},
		"child":                 {c, openParent},
		"child, parent unknown": {c, nil},
		"done child":            {done, openParent},
		"done parent":           {pdone, nil},
		"deeper issue 0":        {tier, &ParentState{Number: 1001, Open: true, Status: Split}},
	} {
		if err := Validate(tc.i, tc.p); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// Render refuses what Validate refuses and renders the status's own comment otherwise.
func TestRenderRefusesAndPicksComment(t *testing.T) {
	bad := childIssue()
	bad.Parent = 0
	if _, err := Render(bad, nil); err == nil {
		t.Fatal("Render accepted a child without a parent")
	}
	r, err := Render(childIssue(), openParent)
	if err != nil {
		t.Fatal(err)
	}
	if r.Comment != StatusComment(childIssue()) || r.Title != Title(childIssue()) {
		t.Fatalf("render: %+v", r)
	}
	if !strings.HasPrefix(r.Body, "Part of #1000\nStack: 1 of 1\nDelivers: ") {
		t.Fatalf("child body must open with Part of / Stack / Delivers, got %q", r.Body)
	}
}

// A split re-renders the parent: its body lists every child in k order with number and status.
func TestSplitReRendersParentChildList(t *testing.T) {
	p := parentIssue()
	p.N = 2
	p.Children = []Child{
		{Number: 1002, K: 2, Summary: "second", Status: Claimed},
		{Number: 1001, K: 1, Summary: "first", Status: Done},
	}
	b := Body(p)
	i1, i2 := strings.Index(b, "#1001"), strings.Index(b, "#1002")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("child list not in k order:\n%s", b)
	}
	if !strings.Contains(SplitComment(p), "split into a stack of 2 pieces") {
		t.Fatalf("split comment: %q", SplitComment(p))
	}
}

func TestFromForm(t *testing.T) {
	nested := map[string]any{"praxis": map[string]any{"topic": "T", "k": float64(1), "n": float64(2), "parent": float64(9), "summary": "s", "delivers": "d", "status": "claimed", "prs": []any{float64(5)}}}
	flat := map[string]any{"praxis.topic": "T", "praxis.k": float64(1), "praxis.n": float64(2), "praxis.parent": float64(9), "praxis.summary": "s", "praxis.delivers": "d", "praxis.status": "claimed", "praxis.prs": []any{float64(5)}}
	for name, f := range map[string]map[string]any{"nested": nested, "flat": flat} {
		i, err := FromForm(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if i.Topic != "T" || i.K != 1 || i.N != 2 || i.Parent != 9 || i.Status != Claimed || len(i.PRs) != 1 || i.PRs[0] != 5 {
			t.Fatalf("%s: %+v", name, i)
		}
	}
}

// The generated doc is pinned by a golden, and every refusal rule appears in it.
func TestGenDoc(t *testing.T) {
	d := GenDoc()
	golden(t, "doc", d)
	for _, r := range []Rule{RuleBadTopic, RuleNOutOfRange, RuleKOutOfRange, RuleNoParent, RuleSelfParent, RuleParentClosed, RuleParentDone, RuleEmptySummary, RuleEmptyDelivers, RuleBadStatus, RuleSplitWithoutChildren, RuleChildOutOfRange, RuleDoneWithoutPRs} {
		if !strings.Contains(d, "`"+string(r)+"`") {
			t.Errorf("doc misses rule %s", r)
		}
	}
}
