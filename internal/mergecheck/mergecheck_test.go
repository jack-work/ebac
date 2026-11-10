package mergecheck

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// golden.json was produced by minister-control's states.py (e65964c) over the live policy file and 21 cases: the
// Go port must give byte-for-byte the same checklist rows and the same state for every one.
type goldenCase struct {
	Name  string           `json:"name"`
	Facts Facts            `json:"facts"`
	Jobs  []Job            `json:"jobs"`
	Row   map[string]any   `json:"row"`
	Item  map[string]any   `json:"item"`
	Tests map[string]int   `json:"tests"`
	Arch  *Arch            `json:"arch"`
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
	if len(g.Cases) < 20 || len(g.Rule) < 500 {
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
			steps = Checklist(p, c.Facts, c.Jobs, Marks{Row: c.Row, Item: c.Item}, tests, c.Arch)
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

// The one deliberate difference from states.py: a test-running job's PASS with an UNKNOWN test count (the hub's
// /jobs/<id>/tests has not answered) is pending, never a pass. A known count passes; 0 tests fails.
func TestUnknownTestCountNeverPasses(t *testing.T) {
	rule, _, _ := loadGolden(t)
	p := Parse(rule)
	f := Facts{Oid: "abc", State: "OPEN", Heavy: true}
	jobs := []Job{{ID: 7, Queue: "aether.gate-kit", Verdict: "PASS"}}
	row := Checklist(p, f, jobs, Marks{}, nil, nil)[0]
	if row.ID != "band-jobs" || row.OK || row.Status != "pending" || !strings.Contains(row.Note, "tests not read yet") {
		t.Fatalf("unknown count: %+v", row)
	}
	if row := Checklist(p, f, jobs, Marks{}, map[int64]int{7: 31}, nil)[0]; !row.OK || !strings.Contains(row.Note, "31 tests") {
		t.Fatalf("known count: %+v", row)
	}
	if row := Checklist(p, f, jobs, Marks{}, map[int64]int{7: 0}, nil)[0]; row.OK || row.Status != "no" {
		t.Fatalf("zero tests: %+v", row)
	}
}

// Drift: a policy step without an evaluator is shown "not automated" and never blocks; an evaluator without a step
// is reported.
func TestPolicyDrift(t *testing.T) {
	p := Parse("# Merge\n\nintro\n\n## threads: threads\nno open threads\n\n## new-step: something new\nprose\n\n## WHEN\nwhenever\n")
	rows := Checklist(p, Facts{Oid: "h", State: "OPEN", Heavy: true}, nil, Marks{}, nil, nil)
	if len(rows) != 2 || rows[1].Status != "manual" || !rows[1].OK || rows[0].Label != "threads" || p.Steps[0].Text != "no open threads" {
		t.Fatalf("rows %+v steps %+v", rows, p.Steps)
	}
	c := SelfCheck(p)
	if strings.Join(c.NotAutomated, ",") != "new-step" || len(c.OrphanEvaluators) != 7 {
		t.Fatalf("self check %+v", c)
	}
}
