package ghcache

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// A3 write-on-change: a write-only round (Pass.Wake) writes the woken
// entity's pending change after the fresh pre-write read, and reads nothing
// else: no ls-refs, no searches, no marks, no other entity. RED if it reads
// on (the cadence's job) or does not write.
func TestAWakeRoundWritesTheWokenEntityAndReadsNothingElse(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	other := *w.st.Entities["widget-pr-8"]
	w.gh.calls, w.gh.lsrefs = nil, 0
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.wake = map[string]bool{"widget-pr-7": true}
	w.round()
	want := []string{`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`}
	if m := w.mutations(); !slices.Equal(m, want) {
		t.Fatalf("mutations %q, want %q", m, want)
	}
	if w.gh.lsrefs != 0 {
		t.Fatalf("a write-only round read %d ls-refs", w.gh.lsrefs)
	}
	for _, q := range w.gh.calls {
		if strings.Contains(q, "search(") || strings.Contains(q, "pullRequest(number:8)") {
			t.Fatalf("a write-only round read beyond the woken entity: %s", q)
		}
	}
	if g := w.formGH("widget-pr-7"); !slices.Contains(g.Labels, "needs-review") {
		t.Fatalf("form after the write: labels %v", g.Labels)
	}
	if e := w.st.Entities["widget-pr-8"]; !e.MarkAt.Equal(other.MarkAt) || !e.DeepAt.Equal(other.DeepAt) || e.Digest != other.Digest {
		t.Fatalf("an entity not woken moved: %+v -> %+v", other, *e)
	}
	for _, f := range w.r.Forms {
		if f.Key != "widget-pr-7" {
			t.Fatalf("a write-only round wrote %s", f.Key)
		}
	}
}

// A wake with nothing to write (ebac's own form write, or a change put back)
// costs no GitHub call at all.
func TestAWakeRoundWithNothingPendingCallsGitHubNotAtAll(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.gh.calls, w.gh.lsrefs = nil, 0
	w.wake = map[string]bool{"widget-pr-7": true}
	r := w.round()
	if len(w.gh.calls) != 0 || w.gh.lsrefs != 0 || len(w.mutations()) != 0 || len(r.Forms) != 0 {
		t.Fatalf("a wake with nothing pending: %d GraphQL call(s) %q, %d ls-refs, %d form write(s)", len(w.gh.calls), w.gh.calls, w.gh.lsrefs, len(r.Forms))
	}
}

// The cadence round after a write-only one runs as ever (reads due marks).
func TestACadenceRoundAfterAWakeReadsAsEver(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.wake = map[string]bool{"widget-pr-7": true}
	w.round()
	w.mutations()
	w.gh.calls, w.gh.lsrefs = nil, 0
	w.now = w.now.Add(ColdEvery)
	w.round()
	if w.gh.lsrefs == 0 || len(w.gh.calls) == 0 {
		t.Fatalf("the cadence round after a wake read nothing: %d ls-refs, %d calls", w.gh.lsrefs, len(w.gh.calls))
	}
}

// A wake of the register alone (an issue ask) runs the creates and reads no
// entity: the ask is recorded, and the next woken round creates the issue.
func TestARegisterWakeCreatesTheAskedIssue(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh := &createGH{now: t0}
	pass := func(at time.Time) Pass {
		return Pass{Config: &Config{Repos: []string{"example/repo"}}, Now: at, Auth: scopeAuth("example/repo"), Ours: "ours",
			Children: map[string]string{}, PraxisNew: ask(nil), Wake: map[string]bool{model.GHCacheRegisterWake: true}}
	}
	r1, err := Run(context.Background(), gh, State{}, pass(t0))
	if err != nil || r1.State.Creates[askID] == nil || r1.State.Creates[askID].State != "creating" || len(gh.mutations) != 0 {
		t.Fatalf("woken round 1 (record): %v %+v mutations %v", err, r1.State.Creates, gh.mutations)
	}
	r2, err := Run(context.Background(), gh, r1.State, pass(t0.Add(2*time.Second)))
	if err != nil || len(gh.mutations) != 1 || !strings.HasPrefix(gh.mutations[0], "createIssue ") || r2.State.Creates[askID].State != "created" {
		t.Fatalf("woken round 2 (create): %v mutations %v creates %+v", err, gh.mutations, r2.State.Creates)
	}
}
