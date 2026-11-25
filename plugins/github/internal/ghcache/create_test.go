package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	praxisstd "github.com/gluck/ebac/internal/praxis"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// createGH is GitHub for the creation tests: our open issues, one subject's
// comments, and the creates it was asked for.
type createGH struct {
	issues    []ourIssue // our open issues in example/repo, newest first
	comments  []map[string]string
	mutations []string // mutation name + vars
	reads     int
	failMut   error
	nextN     int
}

func (f *createGH) GraphQLPartial(_ context.Context, q string) (json.RawMessage, []forge.GQLError, error) {
	f.reads++
	switch {
	case strings.Contains(q, "repo: repository("):
		if !strings.Contains(q, `createdBy:"ours"`) {
			return nil, nil, fmt.Errorf("the listing is not of our issues: %s", q)
		}
		out := map[string]any{"repo": map[string]any{"id": "R_00000001", "issues": map[string]any{"nodes": f.issues}}}
		if strings.Contains(q, "assignee: user(") {
			out["assignee"] = map[string]any{"id": "U_00000001"}
		}
		b, _ := json.Marshal(out)
		return b, nil, nil
	case strings.Contains(q, "subj: node("):
		b, _ := json.Marshal(map[string]any{"subj": map[string]any{"comments": map[string]any{"nodes": f.comments}}})
		return b, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected read %s", q)
}

func (f *createGH) PullHeads(context.Context, string, []int) (map[int]string, error) { return nil, nil }

func (f *createGH) Mutate(_ context.Context, q string, vars map[string]string) (json.RawMessage, error) {
	f.mutations = append(f.mutations, mutationName(q)+" "+vars["b"])
	if f.failMut != nil {
		return nil, f.failMut
	}
	switch {
	case strings.Contains(q, "createIssue"):
		if f.nextN == 0 {
			f.nextN = 1001
		}
		n := f.nextN
		f.nextN++
		f.issues = append([]ourIssue{{Number: n, URL: fmt.Sprintf("https://example.invalid/example/repo/issues/%d", n), Body: vars["b"]}}, f.issues...)
		return json.RawMessage(fmt.Sprintf(`{"createIssue":{"issue":{"number":%d,"url":"https://example.invalid/example/repo/issues/%d"}}}`, n, n)), nil
	case strings.Contains(q, "addComment"):
		id := fmt.Sprintf("IC_0000000%d", len(f.comments)+1)
		f.comments = append(f.comments, map[string]string{"id": id, "url": "https://example.invalid/c/" + id, "body": vars["b"]})
		return json.RawMessage(`{"addComment":{"commentEdge":{"node":{"id":"` + id + `","url":"https://example.invalid/c/` + id + `"}}}}`), nil
	}
	return nil, fmt.Errorf("unexpected mutation %s", q)
}

func scopeAuth(repos ...string) func(model.GHEntity, string) string {
	w := &model.Critic{Write: true, GHWrite: &model.GHWrite{Ours: "ours", Scope: model.GHScope{Repos: repos, Authors: []string{"ours"}},
		Fields: []string{model.FieldIssueCreate, model.FieldCommentCreate}}}
	return func(e model.GHEntity, field string) string { return model.AuthorizeGHWrite(w, e, field) }
}

const askID = "000000000001"

func ask(over map[string]any) map[string]json.RawMessage {
	m := map[string]any{"repo": "example/repo", "topic": "Example topic", "k": 0, "n": 1, "summary": "One outcome", "delivers": "A checkable result",
		"status": "claimed", "assignee": "ours", "by": "@00000001", "at": "2026-11-25T06:00:00Z"}
	for k, v := range over {
		m[k] = v
	}
	return map[string]json.RawMessage{askID: mustJSON(m)}
}

// round runs one creation round as Run does (step 6) and returns the
// creator for its register and the form writes.
func createRound(t *testing.T, gh *createGH, st *State, asks map[string]json.RawMessage, now time.Time, children map[string]string) (*creator, int) {
	t.Helper()
	p := Pass{Now: now, Auth: scopeAuth("example/repo"), Ours: "ours", Children: children}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: now, logf: func(string, ...any) {}, children: children}
	c := &creator{s: sy, st: st, p: p, reg: map[string]any{}}
	fws := c.issues(asks)
	return c, len(fws)
}

func regState(c *creator, id string) map[string]any {
	pn, _ := c.reg["praxis_new"].(map[string]any)
	m, _ := pn[id].(map[string]any)
	return m
}

func TestAnIssueAskIsRecordedOneRoundThenCreatedOnceWithItsMarker(t *testing.T) {
	gh, st, t0 := &createGH{}, &State{}, time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	c, n := createRound(t, gh, st, ask(nil), t0, nil)
	if n != 0 || gh.reads != 0 || len(gh.mutations) != 0 || regState(c, askID)["state"] != "creating" {
		t.Fatalf("round 1 (record only): forms %d reads %d mutations %v register %v", n, gh.reads, gh.mutations, c.reg)
	}
	p := &creator{s: c.s, st: st, p: c.p, reg: map[string]any{}}
	p.p.Now = t0.Add(time.Minute)
	fws := p.issues(ask(nil))
	if len(gh.mutations) != 1 || !strings.HasPrefix(gh.mutations[0], "createIssue ") || !strings.Contains(gh.mutations[0], praxisstd.Marker(askID)) {
		t.Fatalf("round 2: mutations %v", gh.mutations)
	}
	if len(fws) != 1 || fws[0].Key != "repo-issue-1001" || len(fws[0].ExpectAbsent) != 2 {
		t.Fatalf("round 2: the minted form: %+v", fws)
	}
	var px map[string]any
	_ = json.Unmarshal(fws[0].Set["praxis"], &px)
	if px["number"] != float64(1001) || px["id"] != askID || px["summary"] != "One outcome" || px["repo"] != nil {
		t.Fatalf("minted praxis keys: %v", px)
	}
	if s := regState(p, askID); s["state"] != "created" || s["issue"] != 1001 {
		t.Fatalf("register after the create: %v", p.reg)
	}
	q := &creator{s: c.s, st: st, p: c.p, reg: map[string]any{}}
	if fws := q.issues(ask(nil)); len(fws) != 0 || len(gh.mutations) != 1 {
		t.Fatalf("a created ask was created again: %v", gh.mutations)
	}
}

func TestACreateWhoseAnswerWasLostIsAdoptedNotMadeAgain(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	st := &State{}
	gh := &createGH{}
	createRound(t, gh, st, ask(nil), t0, nil)
	// The create went out and GitHub made #1007, but its answer never came back.
	gh.issues = []ourIssue{{Number: 1007, URL: "u", Body: "body\n\n" + praxisstd.Marker(askID)}, {Number: 1006, Body: "another"}}
	c, n := createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	if len(gh.mutations) != 0 || n != 1 || regState(c, askID)["issue"] != 1007 {
		t.Fatalf("adopt: mutations %v forms %d register %v", gh.mutations, n, c.reg)
	}
}

func TestAPraxisRefusalAlarmsAndReachesNoGitHub(t *testing.T) {
	gh, st := &createGH{}, &State{}
	c, _ := createRound(t, gh, st, ask(map[string]any{"summary": " "}), time.Unix(1795000000, 0), nil)
	al, _ := c.reg["alarm"].(map[string]any)
	px, _ := al["praxis"].(map[string]any)
	a, _ := px[askID].(map[string]any)
	if a["rule"] != "empty_summary" || gh.reads+len(gh.mutations) != 0 {
		t.Fatalf("alarm %v, GitHub calls %d", c.reg, gh.reads+len(gh.mutations))
	}
	c2, _ := createRound(t, gh, st, ask(map[string]any{"summary": " "}), time.Unix(1795000600, 0), nil)
	if c2.reg["alarm"] == nil || gh.reads+len(gh.mutations) != 0 {
		t.Fatalf("the refusal stands next round: %v", c2.reg)
	}
}

func TestAnIssueNamingAnInternalTermIsRefused(t *testing.T) {
	gh, st := &createGH{}, &State{}
	c, _ := createRound(t, gh, st, ask(map[string]any{"summary": "0a1b2c3d is the seat that ran it"}), time.Unix(1795000000, 0), nil)
	al, _ := c.reg["alarm"].(map[string]any)
	wr, _ := al[AlarmWriteRefused].(map[string]any)
	if a, _ := wr["praxis_new."+askID].(map[string]any); a["rule"] != "disclose" || gh.reads+len(gh.mutations) != 0 {
		t.Fatalf("register %v, GitHub calls %d", c.reg, gh.reads+len(gh.mutations))
	}
}

func TestAnIssueOutsideTheScopeRepoIsRefused(t *testing.T) {
	gh, st := &createGH{}, &State{}
	c, _ := createRound(t, gh, st, ask(map[string]any{"repo": "example/other"}), time.Unix(1795000000, 0), nil)
	al, _ := c.reg["alarm"].(map[string]any)
	wr, _ := al[AlarmWriteRefused].(map[string]any)
	a, _ := wr["praxis_new."+askID].(map[string]any)
	if a["rule"] != "scope" || !strings.Contains(fmt.Sprint(a["detail"]), "outside the trusted scope") || gh.reads != 0 {
		t.Fatalf("register %v", c.reg)
	}
}

func TestACreatedRecordStaysADayThenGoes(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	kids := map[string]string{"repo-issue-1001": "@000000f1"}
	c, _ := createRound(t, gh, st, nil, t0.Add(2*time.Hour), kids)
	if s := regState(c, askID); s["state"] != "created" || s["form"] != "@000000f1" {
		t.Fatalf("withdrawn ask, created an hour ago: %v", c.reg)
	}
	c, _ = createRound(t, gh, st, nil, t0.Add(26*time.Hour), kids)
	if regState(c, askID) != nil || st.Creates[askID] != nil {
		t.Fatalf("the created record outlived a day: %v %v", c.reg, st.Creates)
	}
	if len(gh.mutations) != 1 {
		t.Fatalf("mutations %v", gh.mutations)
	}
}

func TestAWithdrawnAskThatWasCreatingStillFinishes(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	c, n := createRound(t, gh, st, nil, t0.Add(time.Minute), nil)
	if n != 1 || len(gh.mutations) != 1 || regState(c, askID)["state"] != "created" {
		t.Fatalf("a recorded create was dropped half way: %v %v", gh.mutations, c.reg)
	}
}

// commentRound runs one round of entity k's comment creates.
func commentRound(gh *createGH, e *Entity, want string, now time.Time) (json.RawMessage, map[string]any, *syncer) {
	p := Pass{Now: now, Auth: scopeAuth("example/repo"), Ours: "ours"}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: now, logf: func(string, ...any) {}, children: map[string]string{"repo-pr-7": "@000000e1"}}
	w, shown := sy.comments("repo-pr-7", e, model.GHEntity{Repo: "example/repo", Kind: "pr", N: 7, Author: "ours"}, "PR_00000007", readWant(json.RawMessage(want)))
	return w, shown, sy
}

const wantOne = `{"comments":{"new":{"c1":{"body":"Status: building"}},"keep":true}}`

func TestACommentAskIsRecordedThenPostedOnceWithItsMarker(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{}, &Entity{}
	w, shown, _ := commentRound(gh, e, wantOne, t0)
	if w != nil || gh.reads+len(gh.mutations) != 0 || shown["c1"].(map[string]any)["state"] != "creating" {
		t.Fatalf("round 1: want %s shown %v calls %d", w, shown, gh.reads+len(gh.mutations))
	}
	w, shown, _ = commentRound(gh, e, wantOne, t0.Add(time.Minute))
	marker := CommentMarker("@000000e1", "c1")
	if len(gh.mutations) != 1 || gh.mutations[0] != "addComment "+marker+"\nStatus: building" {
		t.Fatalf("round 2 mutations %q", gh.mutations)
	}
	if string(w) != `{"comments":{"keep":true}}` || shown["c1"].(map[string]any)["id"] != "IC_00000001" {
		t.Fatalf("round 2 want %s shown %v", w, shown)
	}
	if strings.Contains(marker, "@000000e1") || !strings.HasPrefix(marker, "<!-- k:") || len(marker) != len("<!-- k:")+16+len(" -->") {
		t.Fatalf("marker %q", marker)
	}
	commentRound(gh, e, wantOne, t0.Add(2*time.Minute)) // the want removal did not land: still asked
	if len(gh.mutations) != 1 {
		t.Fatalf("posted twice: %q", gh.mutations)
	}
}

func TestALostCommentAnswerIsAdopted(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{}, &Entity{}
	commentRound(gh, e, wantOne, t0)
	gh.comments = []map[string]string{{"id": "IC_00000009", "url": "u", "body": CommentMarker("@000000e1", "c1") + "\nStatus: building"}}
	w, shown, _ := commentRound(gh, e, wantOne, t0.Add(time.Minute))
	if len(gh.mutations) != 0 || shown["c1"].(map[string]any)["id"] != "IC_00000009" || string(w) != `{"comments":{"keep":true}}` {
		t.Fatalf("mutations %v shown %v want %s", gh.mutations, shown, w)
	}
}

func TestACommentNamingAnInternalTermIsRefused(t *testing.T) {
	gh, e := &createGH{}, &Entity{}
	_, shown, _ := commentRound(gh, e, `{"comments":{"new":{"c1":{"body":"0a1b2c3d is the seat that ran it"}}}}`, time.Unix(1795000000, 0))
	if shown["c1"].(map[string]any)["state"] != "refused" || e.Alarms[AlarmWriteRefused+".comments.new.c1"].Detail == "" || gh.reads+len(gh.mutations) != 0 {
		t.Fatalf("shown %v alarms %v calls %d", shown, e.Alarms, gh.reads+len(gh.mutations))
	}
}

func TestACommentOnAnEntityOutsideTheScopeIsRefused(t *testing.T) {
	gh, e := &createGH{}, &Entity{}
	p := Pass{Now: time.Unix(1795000000, 0), Auth: scopeAuth("example/repo")}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: p.Now, logf: func(string, ...any) {}}
	_, shown := sy.comments("other-pr-7", e, model.GHEntity{Repo: "example/other", Kind: "pr", N: 7, Author: "ours"}, "PR_7", readWant(json.RawMessage(wantOne)))
	if shown["c1"].(map[string]any)["state"] != "refused" || gh.reads+len(gh.mutations) != 0 {
		t.Fatalf("shown %v", shown)
	}
}

func TestAMintThatDidNotLandIsWrittenAgainUntilTheFormIsHeld(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, map[string]string{})
	createRound(t, gh, st, ask(nil), t0.Add(time.Minute), map[string]string{})
	_, n := createRound(t, gh, st, ask(nil), t0.Add(2*time.Minute), map[string]string{})
	if n != 1 || len(gh.mutations) != 1 {
		t.Fatalf("an unlanded mint: %d form writes, mutations %v", n, gh.mutations)
	}
	_, n = createRound(t, gh, st, ask(nil), t0.Add(3*time.Minute), map[string]string{"repo-issue-1001": "@000000f1"})
	if n != 0 {
		t.Fatalf("a held form minted again: %d", n)
	}
}
