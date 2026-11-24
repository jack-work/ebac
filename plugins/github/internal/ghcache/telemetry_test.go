package ghcache

import (
	"encoding/json"
	"slices"
	"testing"
)

func whys(r Result) map[int]string {
	out := map[int]string{}
	for _, d := range r.Telemetry.Deep {
		out[d.PR] = d.Why
	}
	return out
}

// Each PR read in full names why, one reason per trigger: first (never read), mark (with the fields that moved),
// unknown-backoff, pending-write; a searched PR also asked by its alias counts as answered twice. RED if a trigger
// is reported under another reason, the moved fields are lost, or the count goes.
func TestTelemetryNamesWhyEachPRIsRead(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if got := whys(r); len(got) != 2 || got[116018] != "first" || got[127740] != "first" {
		t.Fatalf("first round: %v", got)
	}
	if r.Telemetry.Tracked != 2 || r.Telemetry.Searched != 2 || r.Telemetry.AnsweredTwice != 0 || r.Telemetry.FormsWritten != 2 {
		t.Fatalf("first round totals %+v", r.Telemetry)
	}
	pr := gh.prs[c.Repos[0]][127740]
	pr.Mergeable, pr.MergeSt = "CONFLICTING", "DIRTY"
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if len(r.Telemetry.Deep) != 1 || r.Telemetry.Deep[0].Why != "mark" || !slices.Equal(r.Telemetry.Deep[0].Moved, []string{"mergeable", "merge_state"}) {
		t.Fatalf("mark move: %+v", r.Telemetry.Deep)
	}
	if r.Telemetry.Due != 2 || r.Telemetry.AnsweredTwice != 2 {
		t.Fatalf("due %d answered twice %d, want 2 and 2 (both searched and due)", r.Telemetry.Due, r.Telemetry.AnsweredTwice)
	}
	u := gh.prs[c.Repos[0]][116018]
	u.Mergeable, u.MergeSt = "UNKNOWN", "UNKNOWN"
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(2*HotEvery))
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(2*HotEvery+UnknownEvery))
	if got := whys(r); got[116018] != "unknown-backoff" {
		t.Fatalf("backoff: %v", got)
	}

	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", addLabel("needs-review"))
	wr := w.round()
	if got := whys(wr); got[7] != "pending-write" || len(got) != 1 {
		t.Fatalf("pending write: %v", got)
	}
	if ws := wr.Telemetry.Writes; len(ws) != 1 || ws[0] != (WriteRec{PR: "widget-pr-7", Mutation: "addLabelsToLabelable", OK: true}) {
		t.Fatalf("writes %+v", ws)
	}
}

// The round's metrics: names and attributes are the export contract (the report queries them). Literal oracle.
// RED if a name, a kind or an attribute changes.
func TestTelemetryMetrics(t *testing.T) {
	tm := Telemetry{Tracked: 3, Due: 2, FormsWritten: 1, AnsweredTwice: 2, RESTCalls: 0,
		Deep:    []DeepRec{{PR: 1, Why: "mark", Moved: []string{"checks"}}, {PR: 2, Why: "mark", Moved: []string{"checks", "mergeable"}}, {PR: 3, Why: "first"}},
		Writes:  []WriteRec{{PR: "k", Mutation: "resolveReviewThread", OK: true}},
		Alarms:  []string{"k conflict.draft"},
		GraphQL: &Cost{Calls: 3, Points: 11, Left: 4800}}
	tm.metrics()
	b, _ := json.Marshal(tm.Metrics)
	want := `[{"name":"ebac_ghcache_alarms_total","kind":"counter","value":1,"attrs":{"kind":"conflict"}},` +
		`{"name":"ebac_ghcache_answered_twice_total","kind":"counter","value":2},` +
		`{"name":"ebac_ghcache_deep_reads_total","kind":"counter","value":1,"attrs":{"why":"first"}},` +
		`{"name":"ebac_ghcache_deep_reads_total","kind":"counter","value":2,"attrs":{"why":"mark"}},` +
		`{"name":"ebac_ghcache_due","kind":"gauge","value":2},` +
		`{"name":"ebac_ghcache_forms_written_total","kind":"counter","value":1},` +
		`{"name":"ebac_ghcache_graphql_calls_total","kind":"counter","value":3},` +
		`{"name":"ebac_ghcache_graphql_points_left","kind":"gauge","value":4800},` +
		`{"name":"ebac_ghcache_graphql_points_total","kind":"counter","value":11},` +
		`{"name":"ebac_ghcache_mark_moves_total","kind":"counter","value":2,"attrs":{"field":"checks"}},` +
		`{"name":"ebac_ghcache_mark_moves_total","kind":"counter","value":1,"attrs":{"field":"mergeable"}},` +
		`{"name":"ebac_ghcache_rest_calls_total","kind":"counter","value":0},` +
		`{"name":"ebac_ghcache_rounds_total","kind":"counter","value":1,"attrs":{"outcome":"ok"}},` +
		`{"name":"ebac_ghcache_tracked","kind":"gauge","value":3},` +
		`{"name":"ebac_ghcache_writes_total","kind":"counter","value":1,"attrs":{"mutation":"resolveReviewThread","ok":"true"}}]`
	if string(b) != want {
		t.Fatalf("metrics\n got %s\nwant %s", b, want)
	}
}
