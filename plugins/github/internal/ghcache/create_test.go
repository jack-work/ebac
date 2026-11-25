package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	praxisstd "github.com/gluck/ebac/internal/praxis"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// createGH is GitHub for the creation tests: our issues (open and closed,
// newest first), one subject's comments (oldest first), and the creates it
// was asked for; pages of AdoptFirst with index cursors, as GitHub pages.
type createGH struct {
	issues    []ourIssue
	comments  []postedComment
	mutations []string // mutation name + vars
	reads     int
	failMut   error
	partial   bool      // listings answer with a GraphQL error beside the data
	now       time.Time // createdAt of what it creates
	nextN     int
}

var afterRe, beforeRe = regexp.MustCompile(`after:"(\d+)"`), regexp.MustCompile(`before:"(\d+)"`)

func (f *createGH) GraphQLPartial(_ context.Context, q string) (json.RawMessage, []forge.GQLError, error) {
	f.reads++
	var errs []forge.GQLError
	if f.partial {
		errs = []forge.GQLError{{Message: "something went wrong", Path: []string{"repo", "issues"}}}
	}
	switch {
	case strings.Contains(q, "repo: repository("):
		if !strings.Contains(q, `createdBy:"ours"`) || strings.Contains(q, "states:") {
			return nil, nil, fmt.Errorf("the listing is not of all our issues: %s", q)
		}
		from := 0
		if m := afterRe.FindStringSubmatch(q); m != nil {
			from, _ = strconv.Atoi(m[1])
		}
		to := min(from+AdoptFirst, len(f.issues))
		page := f.issues[from:to]
		issues := map[string]any{"nodes": page, "pageInfo": map[string]any{"hasNextPage": to < len(f.issues), "endCursor": strconv.Itoa(to)}}
		out := map[string]any{"repo": map[string]any{"id": "R_00000001", "issues": issues}}
		if f.partial {
			out["repo"].(map[string]any)["issues"] = nil
		}
		if strings.Contains(q, "assignee: user(") {
			out["assignee"] = map[string]any{"id": "U_00000001"}
		}
		b, _ := json.Marshal(out)
		return b, errs, nil
	case strings.Contains(q, "subj: node("):
		to := len(f.comments)
		if m := beforeRe.FindStringSubmatch(q); m != nil {
			to, _ = strconv.Atoi(m[1])
		}
		from := max(0, to-AdoptFirst)
		cs := map[string]any{"nodes": f.comments[from:to], "pageInfo": map[string]any{"hasPreviousPage": from > 0, "startCursor": strconv.Itoa(from)}}
		b, _ := json.Marshal(map[string]any{"subj": map[string]any{"comments": cs}})
		return b, errs, nil
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
		f.issues = append([]ourIssue{{Number: n, URL: fmt.Sprintf("https://example.invalid/example/repo/issues/%d", n), Body: vars["b"], CreatedAt: f.now}}, f.issues...)
		return json.RawMessage(fmt.Sprintf(`{"createIssue":{"issue":{"number":%d,"url":"https://example.invalid/example/repo/issues/%d"}}}`, n, n)), nil
	case strings.Contains(q, "addComment"):
		id := fmt.Sprintf("IC_0000000%d", len(f.comments)+1)
		f.comments = append(f.comments, postedComment{ID: id, URL: "https://example.invalid/c/" + id, Body: vars["b"], CreatedAt: f.now})
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
	if fws := q.issues(ask(nil)); len(fws) != 0 || len(gh.mutations) != 1 || regState(q, askID)["state"] != "created" {
		t.Fatalf("a created ask was asked again: mutations %v register %v", gh.mutations, q.reg)
	}
}

func TestACreateWhoseAnswerWasLostIsAdoptedNotMadeAgain(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	st := &State{}
	gh := &createGH{}
	createRound(t, gh, st, ask(nil), t0, nil)
	// The create went out and GitHub made #1007, but its answer never came back.
	gh.issues = []ourIssue{{Number: 1007, URL: "u", Body: "body\n\n" + praxisstd.Marker(askID), CreatedAt: t0.Add(30 * time.Second)}, {Number: 1006, Body: "another", CreatedAt: t0.Add(-time.Hour)}}
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
	marker := CommentMarker("@000000e1", "c1", "Status: building")
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
	gh.comments = []postedComment{{ID: "IC_00000009", URL: "u", Body: CommentMarker("@000000e1", "c1", "Status: building") + "\nStatus: building", CreatedAt: t0.Add(30 * time.Second)}}
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

// Review r1 (two families): the guards run again at the POST, the adopt
// listing proves absence or nothing is created, and creation state survives
// the production round.

func TestAScopeRevokedAfterTheRecordStopsTheCreate(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	p := Pass{Now: t0.Add(time.Minute), Auth: scopeAuth("example/other"), Ours: "ours"}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: p.Now, logf: func(string, ...any) {}}
	c := &creator{s: sy, st: st, p: p, reg: map[string]any{}}
	c.issues(ask(nil))
	if len(gh.mutations) != 0 || st.Creates[askID].State != "refused" || st.Creates[askID].Rule != "scope" {
		t.Fatalf("a create posted after its scope was revoked: mutations %v state %+v", gh.mutations, st.Creates[askID])
	}
}

func TestAPartialListingCreatesNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{partial: true}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	if len(gh.mutations) != 0 || st.Creates[askID].State != "creating" {
		t.Fatalf("a partial listing was taken as absence: %v %+v", gh.mutations, st.Creates[askID])
	}
}

func TestAnInconclusiveListingCreatesNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	for i := 0; i < AdoptFirst*AdoptPages+1; i++ { // every one newer than the record
		gh.issues = append(gh.issues, ourIssue{Number: 5000 + i, Body: "x", CreatedAt: t0.Add(time.Hour)})
	}
	createRound(t, gh, st, ask(nil), t0, nil)
	createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	if len(gh.mutations) != 0 || gh.reads != AdoptPages {
		t.Fatalf("an inconclusive listing posted: mutations %v reads %d", gh.mutations, gh.reads)
	}
}

func TestALostCreateIsAdoptedFromAnOlderPage(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	for i := 0; i < AdoptFirst+3; i++ {
		gh.issues = append(gh.issues, ourIssue{Number: 6000 + i, Body: "newer", CreatedAt: t0.Add(time.Hour)})
	}
	gh.issues = append(gh.issues, ourIssue{Number: 1007, Body: "b\n" + praxisstd.Marker(askID), CreatedAt: t0.Add(time.Second)},
		ourIssue{Number: 900, Body: "old", CreatedAt: t0.Add(-time.Hour)})
	c, _ := createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	if len(gh.mutations) != 0 || regState(c, askID)["issue"] != 1007 {
		t.Fatalf("the lost create on page 2 was not adopted: %v %v", gh.mutations, c.reg)
	}
}

func TestACreatedIssueIsTrackedAsOurs(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	createRound(t, gh, st, ask(nil), t0.Add(time.Minute), nil)
	e := st.Entities["repo-issue-1001"]
	if e == nil || e.Kind != KindIssue || e.N != 1001 || st.OurIssues["repo-issue-1001"].N != 1001 {
		t.Fatalf("the created issue is not a tracked entity: %+v %+v", e, st.OurIssues)
	}
}

func TestTheCreatesAndOurIssuesCarryAcrossRun(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh := &createGH{now: t0}
	pass := func(at time.Time, kids map[string]string) Pass {
		return Pass{Config: &Config{Repos: []string{"example/repo"}}, Now: at, Auth: scopeAuth("example/repo"), Ours: "ours", Children: kids, PraxisNew: ask(nil)}
	}
	r1, err := Run(context.Background(), gh, State{}, pass(t0, map[string]string{}))
	if err != nil || r1.State.Creates[askID] == nil || r1.State.Creates[askID].State != "creating" || r1.Register == nil {
		t.Fatalf("round 1: %v %+v %v", err, r1.State.Creates, r1.Register)
	}
	r2, err := Run(context.Background(), gh, r1.State, pass(t0.Add(time.Minute), map[string]string{}))
	if err != nil || len(gh.mutations) != 1 || r2.State.Creates[askID].State != "created" || r2.State.OurIssues["repo-issue-1001"].N != 1001 {
		t.Fatalf("round 2: %v mutations %v creates %+v ours %+v", err, gh.mutations, r2.State.Creates, r2.State.OurIssues)
	}
	for _, f := range r2.Forms {
		if f.Retire {
			t.Fatalf("round 2 retired %s", f.Key)
		}
	}
}

func TestARefusedCommentStaysRefused(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{}, &Entity{}
	commentRound(gh, e, wantOne, t0)
	gh.failMut = fmt.Errorf("mutation: Body is too long")
	commentRound(gh, e, wantOne, t0.Add(time.Minute))
	for i := 2; i < 6; i++ {
		commentRound(gh, e, wantOne, t0.Add(time.Duration(i)*time.Minute))
	}
	if len(gh.mutations) != 1 || e.CommentNew["c1"].State != "refused" {
		t.Fatalf("a refused comment was asked again: %v %+v", gh.mutations, e.CommentNew["c1"])
	}
}

func TestAReusedClientKeyWithNewTextIsANewComment(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{now: t0}, &Entity{}
	commentRound(gh, e, wantOne, t0)
	commentRound(gh, e, wantOne, t0.Add(time.Minute))
	commentRound(gh, e, `{}`, t0.Add(2*time.Minute)) // the done ask left the form
	again := `{"comments":{"new":{"c1":{"body":"Status: done"}}}}`
	commentRound(gh, e, again, t0.Add(3*time.Minute))
	commentRound(gh, e, again, t0.Add(4*time.Minute))
	if len(gh.mutations) != 2 || !strings.HasSuffix(gh.mutations[1], "Status: done") {
		t.Fatalf("the new text under a reused key was not posted: %q", gh.mutations)
	}
}

func TestAnAskEditedWhileCreatingPostsTheNewText(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{now: t0}, &Entity{}
	commentRound(gh, e, wantOne, t0)
	edited := `{"comments":{"new":{"c1":{"body":"Status: edited"}},"keep":true}}`
	w, _, _ := commentRound(gh, e, edited, t0.Add(time.Minute)) // re-recorded: nothing posted yet
	if len(gh.mutations) != 0 || w != nil {
		t.Fatalf("the edited ask posted the old text or left the form: %q %s", gh.mutations, w)
	}
	w, _, _ = commentRound(gh, e, edited, t0.Add(2*time.Minute))
	if len(gh.mutations) != 1 || !strings.HasSuffix(gh.mutations[0], "Status: edited") || string(w) != `{"comments":{"keep":true}}` {
		t.Fatalf("mutations %q want %s", gh.mutations, w)
	}
}

func TestACommentOnOurIssueIsPosted(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{now: t0}, &Entity{}
	p := Pass{Now: t0, Auth: scopeAuth("example/repo"), Ours: "ours"}
	run := func(at time.Time) {
		p.Now = at
		sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: at, logf: func(string, ...any) {}, children: map[string]string{"repo-issue-5": "@000000e5"}}
		sy.comments("repo-issue-5", e, model.GHEntity{Repo: "example/repo", Kind: "issue", N: 5, Author: "ours"}, "I_00000005", readWant(json.RawMessage(wantOne)))
	}
	run(t0)
	run(t0.Add(time.Minute))
	if len(gh.mutations) != 1 {
		t.Fatalf("a comment on our own issue was not posted: %v %+v", gh.mutations, e.CommentNew)
	}
}

func TestTheDiscloseListIsAskedAgainAtThePost(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	createRound(t, gh, st, ask(nil), t0, nil)
	p := Pass{Now: t0.Add(time.Minute), Auth: scopeAuth("example/repo"), Ours: "ours", Disclose: func(string) string { return "names a term added to the list since" }}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: p.Now, logf: func(string, ...any) {}}
	(&creator{s: sy, st: st, p: p, reg: map[string]any{}}).issues(ask(nil))
	if len(gh.mutations) != 0 || st.Creates[askID].Rule != "disclose" {
		t.Fatalf("an issue posted past a disclose list tightened since its record: %v %+v", gh.mutations, st.Creates[askID])
	}
	e := &Entity{}
	commentRound(gh, e, wantOne, t0)
	sy2 := &syncer{ctx: context.Background(), gh: gh, p: p, now: p.Now, logf: func(string, ...any) {}, children: map[string]string{"repo-pr-7": "@000000e1"}}
	sy2.comments("repo-pr-7", e, model.GHEntity{Repo: "example/repo", Kind: "pr", N: 7, Author: "ours"}, "PR_00000007", readWant(json.RawMessage(wantOne)))
	if len(gh.mutations) != 0 || e.CommentNew["c1"].State != "refused" {
		t.Fatalf("a comment posted past a disclose list tightened since its record: %v %+v", gh.mutations, e.CommentNew["c1"])
	}
}

func TestACommentScopeRevokedAfterTheRecordStopsThePost(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, e := &createGH{}, &Entity{}
	commentRound(gh, e, wantOne, t0)
	p := Pass{Now: t0.Add(time.Minute), Auth: scopeAuth("example/other")}
	sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: p.Now, logf: func(string, ...any) {}, children: map[string]string{"repo-pr-7": "@000000e1"}}
	sy.comments("repo-pr-7", e, model.GHEntity{Repo: "example/repo", Kind: "pr", N: 7, Author: "ours"}, "PR_00000007", readWant(json.RawMessage(wantOne)))
	if len(gh.mutations) != 0 || e.CommentNew["c1"].State != "refused" {
		t.Fatalf("a comment posted after its scope was revoked: %v %+v", gh.mutations, e.CommentNew["c1"])
	}
}

func TestTheCacheAtItsFormCapCreatesNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	gh, st := &createGH{}, &State{}
	kids := map[string]string{}
	for i := 0; i < 3; i++ {
		kids[fmt.Sprintf("repo-pr-%d", i)] = "@x"
	}
	p := Pass{Config: &Config{Max: 3}, Now: t0, Auth: scopeAuth("example/repo"), Ours: "ours", Children: kids}
	for _, at := range []time.Time{t0, t0.Add(time.Minute)} {
		p.Now = at
		sy := &syncer{ctx: context.Background(), gh: gh, p: p, now: at, logf: func(string, ...any) {}, children: kids}
		(&creator{s: sy, st: st, p: p, reg: map[string]any{}}).issues(ask(nil))
	}
	if len(gh.mutations) != 0 || st.Creates[askID].State != "creating" {
		t.Fatalf("a create past the form cap: %v %+v", gh.mutations, st.Creates[askID])
	}
}
