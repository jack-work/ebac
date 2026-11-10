package boardsync

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestParseTitle(t *testing.T) {
	for title, want := range map[string]string{
		"[Effort ledger][1-173] feat(decision_judge): System1 hooks": "[Effort ledger] 1 173 feat(decision_judge): System1 hooks",
		"[Effort ledger][0-N] docs(design): directive":               "[Effort ledger] 0 0 docs(design): directive",
		"[Prompt cache re-establish][4-13] Read and replace":         "[Prompt cache re-establish] 4 13 Read and replace",
	} {
		tag, k, n, rest, ok := ParseTitle(title)
		if got := fmt.Sprintf("%s %d %d %s", tag, k, n, rest); !ok || got != want {
			t.Errorf("%q: %q ok=%v; want %q", title, got, ok, want)
		}
	}
	for _, title := range []string{"feat(effort): a prediction commits one tier", "[Effort ledger] no index", "fix: [Effort ledger][1-2] not at the start"} {
		if _, _, _, _, ok := ParseTitle(title); ok {
			t.Errorf("%q parsed as tagged", title)
		}
	}
}

func TestParseNeeds(t *testing.T) {
	n, bad := ParseNeeds("# needs-cache.tsv: comment\ncache-1\t#108401\t-\ncache-2\t#108402\t108401\ncache-11\t#108407\t108402,#108405\r\nbroken line\n")
	if len(n[108401]) != 0 || n[108402][0] != 108401 || len(n[108407]) != 2 || n[108407][1] != 108405 {
		t.Fatalf("needs %v", n)
	}
	if _, ok := n[108401]; !ok {
		t.Fatal("a row with - must be present with no needs")
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "line 5") {
		t.Fatalf("bad lines %v", bad)
	}
}

// fixture is the live board's shape on 2026-11-10 with neutral titles: the hand-kept map put the untagged
// one-step #96046 at the head of LEDGER as its front (ledger-6), the split's pieces at ledger-7.., the design doc
// [0-N] at the end; CACHE is not declared in stacks at all; needs.cache holds the Thinker's rows.
func fixture() Input {
	j := func(s string) json.RawMessage { return json.RawMessage(s) }
	in := Input{
		Stacks: map[string]json.RawMessage{
			"LEDGER": j(`{"tag":"[Effort ledger]","order":3}`),
			"GATE":   j(`{"tag":"[Effort router gate]","order":0}`),
			"OTHER":  j(`{"tag":"","order":4}`),
		},
		Prs: map[string]json.RawMessage{
			"ledger-6":   j(`{"front":true,"index":1,"of":175,"pr":96046,"stack":"LEDGER","state":"draft","title":"one-step"}`),
			"ledger-7":   j(`{"front":false,"index":2,"of":175,"pr":108445,"stack":"LEDGER","state":"open","title":"hooks","owner":"@8d70861a"}`),
			"ledger-8":   j(`{"front":false,"index":3,"of":175,"pr":108447,"stack":"LEDGER","state":"draft","title":"terms"}`),
			"ledger-180": j(`{"front":false,"index":175,"of":175,"pr":108984,"stack":"LEDGER","state":"open","title":"[0-N] design"}`),
			"cache-1":    j(`{"front":true,"index":1,"of":13,"pr":108401,"stack":"CACHE","state":"open","title":"c1"}`),
			"cache-2":    j(`{"front":false,"index":2,"of":13,"pr":108402,"stack":"CACHE","state":"draft","title":"c2"}`),
			"cache-3":    j(`{"front":false,"index":3,"of":13,"pr":108405,"stack":"CACHE","state":"open","title":"c3"}`),
			"cache-11":   j(`{"front":false,"index":11,"of":13,"pr":108407,"stack":"CACHE","state":"draft","title":"c11","needs":"cache-2,cache-3"}`),
		},
		PRs: []PR{
			{Number: 96046, Title: "feat(effort): one-step", State: "OPEN", Draft: true},
			{Number: 108445, Title: "[Effort ledger][1-173] hooks", State: "OPEN", Additions: 90},
			{Number: 108447, Title: "[Effort ledger][2-173] terms", State: "OPEN", Draft: true},
			{Number: 108984, Title: "[Effort ledger][0-N] design", State: "OPEN", Additions: 166},
			{Number: 108401, Title: "[Prompt cache re-establish][1-13] c1", State: "OPEN", Additions: 142, PState: "waiting: band jobs"},
			{Number: 108402, Title: "[Prompt cache re-establish][2-13] c2", State: "OPEN", Draft: true},
			{Number: 108405, Title: "[Prompt cache re-establish][3-13] c3", State: "OPEN"},
			{Number: 108407, Title: "[Prompt cache re-establish][11-13] c11", State: "OPEN", Draft: true},
		},
		Needs: map[string]string{
			"cache":  "cache-1\t#108401\t-\ncache-2\t#108402\t108401\ncache-3\t#108405\t-\ncache-11\t#108407\t108402,108405\n",
			"ledger": "# needs-ledger.tsv: not filled yet\n",
		},
		Ledgered: map[int]bool{},
	}
	return in
}

func entry(t *testing.T, r Result, cw string) map[string]any {
	t.Helper()
	var e map[string]any
	if err := json.Unmarshal(r.Prs[cw], &e); err != nil {
		t.Fatalf("%s: %v (%s)", cw, err, r.Prs[cw])
	}
	return e
}

// The ledger split shows its real front (#108445, [1-173]); the untagged one-step #96046 is never a front and goes
// to judgement; the Thinker's needs make cache-1 and cache-3 lane fronts; codewords follow the tags.
func TestReconcileFronts(t *testing.T) {
	r := Reconcile(fixture())
	f := Fronts(r.Prs)
	if strings.Join(f["LEDGER"], ",") != "ledger-0,ledger-1" || strings.Join(f["CACHE"], ",") != "cache-1,cache-3" || len(f["OTHER"]) != 0 {
		t.Fatalf("fronts %v", f)
	}
	if e := entry(t, r, "ledger-1"); e["pr"] != float64(108445) || e["owner"] != "@8d70861a" || e["index"] != float64(1) || e["of"] != float64(173) || e["title"] != "hooks" {
		t.Fatalf("ledger-1 %v (an owner the board added must survive the re-key)", e)
	}
	if e := entry(t, r, "other-96046"); e["front"] != false || e["stack"] != "OTHER" {
		t.Fatalf("#96046 %v", e)
	}
	if e := entry(t, r, "ledger-2"); e["needs"] != "ledger-1" || e["front"] != false {
		t.Fatalf("ledger-2 without a needs row needs the piece before it: %v", e)
	}
	if e := entry(t, r, "cache-11"); e["needs"] != "cache-2,cache-3" || e["front"] != false {
		t.Fatalf("cache-11 %v", e)
	}
	if !strings.Contains(strings.Join(r.Judgement, "\n"), "#96046 has no stack tag") {
		t.Fatalf("judgement %v", r.Judgement)
	}
	for _, cw := range []string{"ledger-6", "ledger-7", "ledger-8", "ledger-180"} {
		if _, ok := r.Prs[cw]; ok {
			t.Errorf("stale codeword %s kept", cw)
		}
	}
	if !r.Dirty() {
		t.Fatal("a corrected board is not dirty")
	}
}

// A second pass over its own output changes nothing (written only on change).
func TestReconcileIsAFixedPoint(t *testing.T) {
	in := fixture()
	r := Reconcile(in)
	in.Prs = r.Prs
	r2 := Reconcile(in)
	if r2.Dirty() {
		t.Fatalf("second pass: added %v changed %v removed %v", r2.Added, r2.Changed, r2.Removed)
	}
}

// A merge moves the front; the merged piece stays until its ledger record exists, then it is archived.
func TestReconcileMergeAndArchive(t *testing.T) {
	in := fixture()
	in.Prs = Reconcile(in).Prs
	in.PRs[1].State = "MERGED" // #108445, ledger-1
	r := Reconcile(in)
	if e := entry(t, r, "ledger-1"); e["state"] != "merged" || e["front"] != false {
		t.Fatalf("merged ledger-1 %v", e)
	}
	if f := Fronts(r.Prs); strings.Join(f["LEDGER"], ",") != "ledger-0,ledger-2" {
		t.Fatalf("front after the merge %v", f)
	}
	in.Prs = r.Prs
	in.Ledgered[108445] = true
	r = Reconcile(in)
	if _, ok := r.Prs["ledger-1"]; ok || strings.Join(r.Removed, ",") != "ledger-1" {
		t.Fatalf("ledgered piece not archived: removed %v", r.Removed)
	}
}

// A title tag that moves a PR to another stack re-lanes it; an unknown tag and a claimed index go to judgement.
func TestReconcileReLaneAndJudgement(t *testing.T) {
	in := fixture()
	in.Prs = Reconcile(in).Prs
	in.PRs[5].Title = "[Effort router gate][7-10] c2 moved" // #108402
	in.PRs = append(in.PRs, PR{Number: 200001, Title: "[Brand new stack][1-2] x", State: "OPEN"}, PR{Number: 200002, Title: "[Effort ledger][1-173] dup", State: "OPEN"})
	r := Reconcile(in)
	if e := entry(t, r, "gate-7"); e["pr"] != float64(108402) || e["stack"] != "GATE" {
		t.Fatalf("re-lane %v", e)
	}
	if _, ok := r.Prs["cache-2"]; ok {
		t.Fatal("old lane entry kept")
	}
	j := strings.Join(r.Judgement, "\n")
	if !strings.Contains(j, "tag [Brand new stack] names no board stack") || !strings.Contains(j, "ledger-1 claimed by") {
		t.Fatalf("judgement %v", r.Judgement)
	}
}

// Against a copy of the live board and our open PRs (EBAC_BOARD_LIVE=<dir> holding board.json from
// `figaro state @zombie-phrasing -j` and open.json from `gh pr list --json number,title,isDraft`): the ledger
// front is #108445, #96046 is never a front, and every open PR lands on the board.
func TestLiveBoardCopy(t *testing.T) {
	dir := os.Getenv("EBAC_BOARD_LIVE")
	if dir == "" {
		t.Skip("set EBAC_BOARD_LIVE to a directory with board.json and open.json")
	}
	var board struct {
		Prs    map[string]json.RawMessage `json:"prs"`
		Stacks map[string]json.RawMessage `json:"stacks"`
	}
	var open []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		IsDraft bool   `json:"isDraft"`
	}
	for f, v := range map[string]any{"board.json": &board, "open.json": &open} {
		b, err := os.ReadFile(dir + "/" + f)
		if err != nil || len(b) < 100 {
			t.Fatalf("%s: %v (%d bytes)", f, err, len(b))
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{Prs: board.Prs, Stacks: board.Stacks, Needs: map[string]string{}, Ledgered: map[int]bool{}}
	for _, o := range open {
		in.PRs = append(in.PRs, PR{Number: o.Number, Title: o.Title, State: "OPEN", Draft: o.IsDraft})
	}
	for _, s := range []string{"cache", "ledger"} {
		if b, err := os.ReadFile(dir + "/needs-" + s + ".tsv"); err == nil {
			in.Needs[s] = string(b)
		}
	}
	r := Reconcile(in)
	f := Fronts(r.Prs)
	t.Logf("open %d, board %d -> %d entries; added %d changed %d removed %d", len(open), len(board.Prs), len(r.Prs), len(r.Added), len(r.Changed), len(r.Removed))
	for k, v := range f {
		var prs []string
		for _, cw := range v {
			var e struct {
				PR int `json:"pr"`
			}
			_ = json.Unmarshal(r.Prs[cw], &e)
			prs = append(prs, fmt.Sprintf("%s #%d", cw, e.PR))
		}
		t.Logf("fronts %s: %s", k, strings.Join(prs, ", "))
	}
	for _, l := range r.Judgement {
		t.Logf("judgement: %s", l)
	}
	if e := entry(t, r, "ledger-1"); e["pr"] != float64(108445) || e["front"] != true {
		t.Fatalf("ledger-1 %v", e)
	}
	for _, cw := range f["LEDGER"] {
		var e struct {
			PR int `json:"pr"`
		}
		_ = json.Unmarshal(r.Prs[cw], &e)
		if e.PR == 96046 {
			t.Fatal("#96046 shown as a front")
		}
	}
	if len(r.Prs) != len(open) {
		t.Fatalf("%d open PRs, %d board entries", len(open), len(r.Prs))
	}
}
