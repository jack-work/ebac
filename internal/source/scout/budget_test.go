package scout

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// synthetic is a world of prs PRs and issues issues shaped like the live scout's (2026-11-11: 83 PRs, 60 issues; title
// ~95 B, an architect verdict on a third, lukas on a fifth, the automaton on a tenth, a failing check on a third,
// check-ni on half the heads and an opt-in gate on a tenth), bodies linking issues, issues with labels, a checklist
// and human comments; a fifth of the PRs merged after the first pass.
func synthetic(t *testing.T, prs, issues int, merge bool) *world {
	w := newWorld(t)
	w.gh.items = map[int]*item{}
	w.hub.jobs = nil
	old := w.now.Add(-time.Hour)
	merged := map[int]bool{}
	for i := 0; i < prs; i++ {
		n := 107000 + i
		head := fmt.Sprintf("%040x", n)
		it := &item{n: n, author: []string{ours, his}[i%2], state: "OPEN", head: head, mergeable: "MERGEABLE", updated: old, unresolved: i % 3,
			title: fmt.Sprintf("[Effort dispatch overlaid modelId][%d-%d] The dispatch reads the handed modelId from the catalog", i%9+1, 9),
			body:  strings.Repeat("prose ", 200),
			files: []string{"aether_rust/crates/aether_services/src/decision_judge/effort_router.rs", "aether_rust/crates/x/src/lib.rs"},
			checks: []map[string]any{{"name": "Aether Runtime PR Pipeline", "conclusion": "SUCCESS"}, {"name": "Aether Rust PR Pipeline", "conclusion": "SUCCESS"},
				{"name": "license/cla", "conclusion": "SUCCESS"}, {"name": "CodeQL", "conclusion": "SUCCESS"}, {"name": "docs", "conclusion": "SKIPPED"}}}
		if issues > 0 {
			it.body = fmt.Sprintf("Part of #%d and #%d. ", 3000+i%issues, 3000+(i+1)%issues) + it.body
		}
		if i%3 == 0 {
			it.checks[1]["conclusion"] = "FAILURE"
			it.comments = append(it.comments, map[string]any{"author": map[string]any{"login": "architect-review"}, "createdAt": "2026-11-10T20:00:00Z",
				"body": fmt.Sprintf("<!-- architect-review -->\nArchitect review \u2014 PR #%d @ `%s`\n\n**APPROVE_WITH_NOTES**\n", n, head[:8])})
		}
		if i%10 == 0 {
			it.comments = append(it.comments, map[string]any{"author": map[string]any{"login": "bob"}, "createdAt": "2026-11-10T21:00:00Z",
				"body": fmt.Sprintf("<!-- automaton:%d:%s verdict:approve -->", n, head[:8])})
			w.hub.jobs = append(w.hub.jobs, hubJob(int64(800000+i), "aether.gate-kit", head, "done", true))
			w.hub.tests[int64(800000+i)] = 1234
		}
		if i%5 == 0 {
			it.reviews = []map[string]any{{"author": map[string]any{"login": "lukas"}, "state": "APPROVED", "commit": map[string]any{"oid": head}}}
		}
		if i%2 == 0 {
			w.hub.jobs = append(w.hub.jobs, hubJob(int64(900000+i), "aether.check-ni", head, "done", true))
		}
		if merge && i%5 == 4 {
			merged[n] = true
		}
		w.gh.items[n] = it
	}
	for i := 0; i < issues; i++ {
		n := 3000 + i
		w.gh.items[n] = &item{n: n, issue: true, author: ours, state: "OPEN", updated: old, labels: []string{"effort-router", "plan"},
			title: fmt.Sprintf("Effort plan item %d: route the dispatch through the catalog's modelId", i),
			body:  "The plan item.\n\n- [ ] one\n- [x] two\n- [ ] three\n" + strings.Repeat("words ", 100),
			comments: []map[string]any{{"author": who("alice"), "createdAt": "2026-11-10T20:00:00Z"},
				{"author": who("bob"), "createdAt": "2026-11-10T21:00:00Z"}}}
	}
	w.pass(0)
	for n := range merged {
		w.gh.items[n].state, w.gh.items[n].mergedAt, w.gh.items[n].updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	}
	w.pass(2 * time.Minute)
	return w
}

// The leader's target: today's volume (83 PRs, 60 issues) under 64 KB with every entry, the state and the overhead.
func TestTodayUnder64KB(t *testing.T) {
	w := synthetic(t, 83, 60, true)
	total, _ := FormSize(&w.st)
	if len(w.st.PRs) != 83 || len(w.st.Issues) != 60 || total >= 64<<10 {
		t.Fatalf("%d PRs, %d issues: %d B (%.1f KB), want < 64 KB", len(w.st.PRs), len(w.st.Issues), total, float64(total)/1024)
	}
	t.Logf("143 entries: %.1f KB (state %.1f KB)", float64(total)/1024, float64(len(model.MustJSON(w.st)))/1024)
}

// Twice today's volume and more (300 entries) stays under the cap with nothing pruned or held.
func TestThreeHundredUnderTheCap(t *testing.T) {
	w := synthetic(t, 180, 120, true)
	total, _ := FormSize(&w.st)
	if len(w.st.PRs) != 180 || len(w.st.Issues) != 120 || total >= FormCapBytes || w.st.Warning != "" && w.st.Warning != digest("") {
		t.Fatalf("%d PRs, %d issues: %.1f KB, warning %q", len(w.st.PRs), len(w.st.Issues), float64(total)/1024, w.st.Warning)
	}
	t.Logf("300 entries: %.1f KB", float64(total)/1024)
}

// Past the cap: merged entries go first, oldest first, only with their ledger record sent; the warning key says so,
// and goes when the form is back under; open entries are never pruned.
func TestPastTheCapPrunesOldestClosed(t *testing.T) {
	w := synthetic(t, 40, 10, true)
	total, open := FormSize(&w.st)
	closed := 0
	for _, tp := range w.st.PRs {
		if tp.State != "OPEN" {
			closed++
			if tp.Record == "" {
				t.Fatal("a merged PR without its record")
			}
		}
	}
	// Make two merges older than the rest, then cap the form just under its size.
	var oldest []int
	for n, tp := range w.st.PRs {
		if tp.State != "OPEN" && len(oldest) < 2 {
			tp.Done -= 3600 // and upstream too: the search still returns them for CursorSkew
			w.gh.items[n].mergedAt = time.Unix(tp.Done, 0).UTC().Format(time.RFC3339)
			oldest = append(oldest, n)
		}
	}
	w.cap = total - 200
	r := w.pass(2 * time.Minute)
	if len(r.Unset) < 1 || !strings.HasPrefix(r.Unset[0], "pr:") || r.Set["warning"] == nil {
		t.Fatalf("no prune: unset %v, set %v", r.Unset, setKeys(r))
	}
	for _, k := range r.Unset {
		var n int
		fmt.Sscanf(k, "pr:%d", &n)
		if n != oldest[0] && n != oldest[1] || w.st.PRs[n] != nil {
			t.Fatalf("pruned %s, not one of the oldest %v", k, oldest)
		}
	}
	var warn map[string]string
	_ = json.Unmarshal(r.Set["warning"], &warn)
	if !strings.Contains(warn["text"], "pruned oldest first") || open > w.cap {
		t.Fatalf("warning %v", warn)
	}
	w.cap = 0
	if r := w.pass(2 * time.Minute); strings.Join(r.Unset, " ") != "warning" {
		t.Fatalf("warning not cleared: %v", r.Unset)
	}
	_ = closed
}

// Open entries alone past the cap: new entries are held back (not written, re-read later), the warning stands, and
// the warn role is pinged once; when room returns the held entries are written.
func TestOpenAlonePastTheCapHoldsAndPingsOnce(t *testing.T) {
	w := synthetic(t, 20, 0, false)
	w.watch.WarnTo = "@2ea091e5000000000000000000000000"
	total, _ := FormSize(&w.st)
	w.cap = total + 1200 // room for some of the new entries, not all
	for i := 0; i < 5; i++ {
		n := 108000 + i
		w.gh.items[n] = &item{n: n, author: ours, state: "OPEN", head: fmt.Sprintf("%040x", n), mergeable: "MERGEABLE", updated: w.now.Add(time.Minute),
			title: "[Effort][1-1] a new piece with a long enough title to cost what titles cost on this form"}
	}
	r := w.pass(2 * time.Minute)
	held := 0
	for _, tp := range w.st.PRs {
		if tp.Held {
			held++
		}
	}
	if held < 1 || held == 5 || r.Set["warning"] == nil {
		t.Fatalf("held %d, set %v", held, setKeys(r))
	}
	if total, _ := FormSize(&w.st); total > w.cap {
		t.Fatalf("form %d B past the cap %d B", total, w.cap)
	}
	// Open alone past a cap now set below them: one ping, not two.
	w.cap = 2 << 10
	r = w.pass(2 * time.Minute)
	if len(r.Pings) != 1 || r.Pings[0].To != w.watch.WarnTo || !strings.Contains(r.Pings[0].Text, "open entries alone") {
		t.Fatalf("pings %v", r.Pings)
	}
	if r := w.pass(2 * time.Minute); len(r.Pings) != 0 || containsStr(r.Unset, "warning") {
		t.Fatalf("pinged again (%v) or the warning went while still past the cap (%v)", r.Pings, r.Unset)
	}
	// Room again: the held entries are re-read and written.
	w.cap = 0
	r = w.pass(2 * time.Minute)
	for n := 108000; n < 108005; n++ {
		if w.st.PRs[n].Held {
			t.Fatalf("#%d still held after room returned (set %v)", n, setKeys(r))
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Past the cap with nothing to prune and nothing new: the warning still stands.
func TestWarningStandsAtTheCap(t *testing.T) {
	w := synthetic(t, 10, 0, false)
	w.cap = 1 << 10
	r := w.pass(2 * time.Minute)
	if r.Set["warning"] == nil || len(r.Unset) != 0 {
		t.Fatalf("at the cap, nothing to do: set %v unset %v", setKeys(r), r.Unset)
	}
}
