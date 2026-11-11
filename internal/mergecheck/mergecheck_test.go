package mergecheck

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// golden.json was produced by minister-control's states.py (846872c) over the policy file and 34 cases
// (testdata/gen_golden.py): the Go port must give byte-for-byte the same checklist rows and the same state for every
// one.
type goldenCase struct {
	Name  string           `json:"name"`
	Facts Facts            `json:"facts"`
	Jobs  []Job            `json:"jobs"`
	Row   map[string]any   `json:"row"`
	Item  map[string]any   `json:"item"`
	Tests map[string]int   `json:"tests"`
	Arch  *Arch            `json:"arch"`
	Files []string         `json:"files"` // null: not known
	Steps *json.RawMessage `json:"steps"`
	State string           `json:"state"`
}

func loadGolden(t *testing.T) (string, Check, []goldenCase) {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Rule      string       `json:"rule"`
		SelfCheck Check        `json:"self_check"`
		Cases     []goldenCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 30 || len(g.Rule) < 500 {
		t.Fatalf("golden too small: %d cases, rule %d B: an empty input proves nothing", len(g.Cases), len(g.Rule))
	}
	return g.Rule, g.SelfCheck, g.Cases
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(x)
	return string(out)
}

func TestParityWithStatesPy(t *testing.T) {
	rule, check, cases := loadGolden(t)
	p := Parse(rule)
	if len(p.Steps) != 8 || p.Steps[0].ID != "band-jobs" || p.Title == "" {
		t.Fatalf("policy: %d steps, first %+v, title %q", len(p.Steps), p.Steps, p.Title)
	}
	if got := SelfCheck(p); !reflect.DeepEqual(got, check) {
		t.Fatalf("self check %+v; states.py %+v", got, check)
	}
	for _, c := range cases {
		tests := map[int64]int{}
		for k, v := range c.Tests {
			id, _ := strconv.ParseInt(k, 10, 64)
			tests[id] = v
		}
		var steps []Row
		if c.Facts.Heavy && c.Facts.State == "OPEN" {
			steps = Checklist(p, c.Facts, c.Jobs, Marks{Row: c.Row, Item: c.Item}, tests, c.Arch, c.Files)
		}
		want := "null"
		if c.Steps != nil {
			want = canonical(t, c.Steps)
		}
		if got := canonical(t, steps); steps != nil && got != want || steps == nil && want != "null" {
			t.Errorf("%s: checklist differs\n go: %s\n py: %s", c.Name, got, want)
		}
		if got := StateOf(&c.Facts, steps); got != c.State {
			t.Errorf("%s: state %q; states.py %q", c.Name, got, c.State)
		}
	}
}

// The required band jobs are the policy's, parsed from the band-jobs step: today's text requires check-ni only
// (clippy-pr is exempt for an open PR head, the gates are opt-in); the text of the morning (check-ni + clippy-pr at the
// head) requires both, and the checklist follows whichever text it is given.
func TestRequiredJobsFollowTheRule(t *testing.T) {
	rule, _, _ := loadGolden(t)
	if got := strings.Join(RequiredJobs(Parse(rule)), " "); got != "aether.check-ni" {
		t.Fatalf("today's rule requires %q", got)
	}
	morning := "# M\n\nintro\n\n## band-jobs: band jobs\n1. Band runs fast pre-checks only: aether.check-ni (compile incl. tests) + " +
		"aether.clippy-pr at the head. Read the result on every PASS. gate-ext / decision-build are opt-in, never a default merge requirement.\n"
	p := Parse(morning)
	if got := strings.Join(RequiredJobs(p), " "); got != "aether.check-ni aether.clippy-pr" {
		t.Fatalf("morning rule requires %q", got)
	}
	f := Facts{Oid: "abc", State: "OPEN", Heavy: true}
	ni := []Job{{ID: 1, Queue: "aether.check-ni", Verdict: "PASS"}}
	if row := Checklist(p, f, ni, Marks{}, nil, nil, []string{"a.rs"})[0]; row.OK || !strings.Contains(row.Note, "clippy-pr: not run on this head") {
		t.Fatalf("morning rule without clippy-pr: %+v", row)
	}
	if row := Checklist(Parse(rule), f, ni, Marks{}, nil, nil, []string{"a.rs"})[0]; !row.OK {
		t.Fatalf("today's rule with check-ni PASS: %+v", row)
	}
	// A queue the rule makes required is required, fast or not.
	lints := Parse("# M\n\nintro\n\n## band-jobs: band jobs\naether.check-ni + aether.decision-build at the head.\n")
	if got := strings.Join(RequiredJobs(lints), " "); got != "aether.check-ni aether.decision-build" {
		t.Fatalf("decision-build required: %q", got)
	}
	if row := Checklist(lints, f, ni, Marks{}, nil, nil, []string{"a.rs"})[0]; row.OK || !strings.Contains(row.Note, "decision-build: not run") {
		t.Fatalf("decision-build missing: %+v", row)
	}
	db := append([]Job{{ID: 2, Queue: "aether.decision-build", Verdict: "PASS"}}, ni...)
	if row := Checklist(lints, f, db, Marks{}, nil, nil, []string{"a.rs"})[0]; !row.OK {
		t.Fatalf("decision-build PASS: %+v", row)
	}
	optin := "# M\n\nintro\n\n## band-jobs: band jobs\naether.check-ni at the head; aether.decision-build lints=1 is opt-in.\n"
	if got := strings.Join(RequiredJobs(Parse(optin)), " "); got != "aether.check-ni" {
		t.Fatalf("opt-in named: %q", got)
	}
}

// Drift: a policy step without an evaluator is shown "not automated" and never blocks; an evaluator without a step
// is reported.
func TestPolicyDrift(t *testing.T) {
	p := Parse("# Merge\n\nintro\n\n## threads: threads\nno open threads\n\n## new-step: something new\nprose\n\n## WHEN\nwhenever\n")
	rows := Checklist(p, Facts{Oid: "h", State: "OPEN", Heavy: true}, nil, Marks{}, nil, nil, nil)
	if len(rows) != 2 || rows[1].Status != "manual" || !rows[1].OK || rows[0].Label != "threads" || p.Steps[0].Text != "no open threads" {
		t.Fatalf("rows %+v steps %+v", rows, p.Steps)
	}
	c := SelfCheck(p)
	if strings.Join(c.NotAutomated, ",") != "new-step" || len(c.OrphanEvaluators) != 7 {
		t.Fatalf("self check %+v", c)
	}
}
