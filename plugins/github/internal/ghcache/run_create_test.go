package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/poll"
	praxisstd "github.com/gluck/ebac/internal/praxis"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// Run-level proofs of the review r2 blockers (strategist ruling): each runs
// the whole round and judges its reply as the core does (poll.ValidateForms).

// comboGH is the PR/issue world (fakeGH) plus the creation calls (createGH):
// our-issue listings, comment listings, createIssue and addComment go to the
// latter, everything else to the former.
type comboGH struct {
	*fakeGH
	c *createGH
}

func (g comboGH) GraphQLPartial(ctx context.Context, q string) (json.RawMessage, []forge.GQLError, error) {
	if strings.Contains(q, "repo: repository(") || strings.Contains(q, "subj: node(") {
		return g.c.GraphQLPartial(ctx, q)
	}
	return g.fakeGH.GraphQLPartial(ctx, q)
}

func (g comboGH) Mutate(ctx context.Context, q string, vars map[string]string) (json.RawMessage, error) {
	if strings.Contains(q, "createIssue") || strings.Contains(q, "addComment") {
		return g.c.Mutate(ctx, q, vars)
	}
	return g.fakeGH.Mutate(ctx, q, vars)
}

func ghcCritic(kids map[string]string) *model.Critic {
	return &model.Critic{Name: "ghc", Source: model.SourceGHCache, Runtime: model.Runtime{Children: kids}}
}

func askIn(repo, id string, over map[string]any) json.RawMessage {
	m := map[string]any{"repo": repo, "topic": "Example topic", "k": 0, "n": 1, "summary": "One outcome", "delivers": "A checkable result",
		"status": "claimed", "by": "@00000001", "at": "2026-11-25T06:00:00Z"}
	for k, v := range over {
		m[k] = v
	}
	return mustJSON(m)
}

func authFor(repo string) func(model.GHEntity, string) string {
	w := &model.Critic{Write: true, GHWrite: &model.GHWrite{Ours: "ours", Scope: model.GHScope{Repos: []string{repo}, Authors: []string{"ours"}},
		Fields: []string{model.FieldIssueCreate, model.FieldCommentCreate}}}
	return func(e model.GHEntity, field string) string { return model.AuthorizeGHWrite(w, e, field) }
}

func copyKids(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// claude r2 B1: the issue's form was minted in a round whose state was lost;
// the next round adopts the issue. The reply must pass the core: no praxis
// on the held form, no retire of it.
func TestRunAdoptOnAHeldIssueFormPassesTheCore(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	cg := &createGH{now: t0}
	gh := comboGH{fakeGH: issueWorld(t), c: cg}
	asks := map[string]json.RawMessage{askID: askIn("example/repo", askID, nil)}
	c := &Config{Repos: []string{"example/repo"}}
	r1, err := Run(context.Background(), gh, State{}, Pass{Config: c, Now: t0, Auth: authFor("example/repo"), Ours: "ours", Children: map[string]string{}, PraxisNew: asks})
	if err != nil {
		t.Fatal(err)
	}
	// Round 2 created #1001 and its mint landed, but the critic's save failed: round 3 runs on round 1's state.
	cg.issues = []ourIssue{{Number: 1001, URL: "u", Body: "b\n" + praxisstd.Marker(askID), CreatedAt: t0.Add(time.Minute)}}
	kids := map[string]string{"repo-issue-1001": "@000000f1"}
	r3, err := Run(context.Background(), gh, r1.State, Pass{Config: c, Now: t0.Add(2 * time.Minute), Auth: authFor("example/repo"), Ours: "ours", Children: kids, PraxisNew: asks})
	if err != nil {
		t.Fatal(err)
	}
	if err := poll.ValidateForms(ghcCritic(kids), &plugin.Reply{Forms: r3.Forms}); err != nil {
		t.Fatalf("the core refuses the adopting round's reply: %v", err)
	}
	for _, f := range r3.Forms {
		if f.Key == "repo-issue-1001" && (f.Retire || f.Set["praxis"] != nil) {
			t.Fatalf("the adopted issue's held form was retired or re-minted: %+v", f)
		}
	}
	if len(cg.mutations) != 0 {
		t.Fatalf("an adopted issue was created again: %v", cg.mutations)
	}
}

// claude r2 B2: a created issue that left tracking (closed past its TTL: its
// entity, our record and its form gone) is not minted again while its ask
// still stands.
func TestRunACreatedIssueThatLeftTrackingIsNotMintedAgain(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	cg := &createGH{now: t0}
	gh := comboGH{fakeGH: issueWorld(t), c: cg}
	asks := map[string]json.RawMessage{askID: askIn("example/repo", askID, nil)}
	c := &Config{Repos: []string{"example/repo"}}
	st := State{}
	for i := 0; i < 2; i++ {
		r, err := Run(context.Background(), gh, st, Pass{Config: c, Now: t0.Add(time.Duration(i) * time.Minute), Auth: authFor("example/repo"), Ours: "ours", Children: map[string]string{}, PraxisNew: asks})
		if err != nil {
			t.Fatal(err)
		}
		st = r.State
	}
	if st.Creates[askID] == nil || st.Creates[askID].State != "created" {
		t.Fatalf("not created: %+v", st.Creates[askID])
	}
	// The issue closed and its TTL passed: Run dropped the entity and our record, and retired the form.
	delete(st.Entities, "repo-issue-1001")
	delete(st.OurIssues, "repo-issue-1001")
	for i := 2; i < 4; i++ {
		r, err := Run(context.Background(), gh, st, Pass{Config: c, Now: t0.Add(time.Duration(i) * time.Minute), Auth: authFor("example/repo"), Ours: "ours", Children: map[string]string{}, PraxisNew: asks})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range r.Forms {
			if f.Key == "repo-issue-1001" {
				t.Fatalf("round %d minted the untracked issue's form again: %+v", i, f)
			}
		}
		st = r.State
	}
}

// claude r2 B3: an ask whose rendered body passes GitHub's limit is refused
// at its record, and the state the round saves stays small.
func TestRunAHugeAskIsRefusedAndTheStateStaysSmall(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	cg := &createGH{now: t0}
	gh := comboGH{fakeGH: issueWorld(t), c: cg}
	asks := map[string]json.RawMessage{askID: askIn("example/repo", askID, map[string]any{"contract": strings.Repeat("Contract line\n", 100000/14)})}
	c := &Config{Repos: []string{"example/repo"}}
	r, err := Run(context.Background(), gh, State{}, Pass{Config: c, Now: t0, Auth: authFor("example/repo"), Ours: "ours", Children: map[string]string{}, PraxisNew: asks})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mustJSON(r.State)); n > 16<<10 {
		t.Fatalf("a 100 KB ask put %d bytes into the cache's state", n)
	}
	if cr := r.State.Creates[askID]; cr == nil || cr.State != "refused" {
		t.Fatalf("the 100 KB ask was not refused at its record: %+v", cr)
	}
	if len(cg.mutations)+cg.reads != 0 {
		t.Fatalf("GitHub was asked: %v", cg.mutations)
	}
}

// claude r2 B4 / gpt r2 B1: max 90, 70 held, 20 fresh entity mints and 11
// creations due in one round: the reply must stay within the cap.
func TestRunTheChildBudgetHoldsWithFreshMintsAndCreates(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	prs := map[int]*PR{}
	var nums []string
	for n := 1; n <= 90; n++ {
		prs[n] = synthPR(t, n, "our-author", nil, false, 0)
		nums = append(nums, fmt.Sprint(n))
	}
	fg := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{}, prs: map[string]map[int]*PR{"acme/widget": prs}}
	kids := map[string]string{}
	for n := 1; n <= 70; n++ {
		kids[ChildKey("acme/widget", n)] = fmt.Sprintf("@%08d", n)
	}
	cg := &createGH{now: t0}
	gh := comboGH{fakeGH: fg, c: cg}
	c, _ := cfg(t, `{"repos":["acme/widget"],"max":90,"rules":[{"repo":"acme/widget","prs":[`+strings.Join(nums, ",")+`]}]}`)
	st := State{Creates: map[string]*Create{}}
	asks := map[string]json.RawMessage{}
	for i := 1; i <= 11; i++ {
		id := fmt.Sprintf("%012d", i)
		raw := askIn("acme/widget", id, nil)
		asks[id] = raw
		st.Creates[id] = &Create{Repo: "acme/widget", State: "creating", At: t0, Req: hashJSON(raw), Title: "t", Body: "b\n" + praxisstd.Marker(id), Praxis: raw}
	}
	r, err := Run(context.Background(), gh, st, Pass{Config: c, Now: t0.Add(time.Minute), Auth: authFor("acme/widget"), Ours: "ours", Children: copyKids(kids), PraxisNew: asks})
	if err != nil {
		t.Fatal(err)
	}
	if err := poll.ValidateForms(ghcCritic(copyKids(kids)), &plugin.Reply{Forms: r.Forms}); err != nil {
		t.Fatalf("the core refuses the round's reply: %v (creates posted: %d)", err, len(cg.mutations))
	}
	if len(cg.mutations) != 0 {
		t.Fatalf("%d creates posted with no form slot left (70 held + 20 fresh of max 90; %d form writes)", len(cg.mutations), len(r.Forms))
	}
	set := copyKids(kids)
	for _, f := range r.Forms {
		if f.Retire {
			delete(set, f.Key)
		} else {
			set[f.Key] = "x"
		}
	}
	if len(set) > 90 {
		t.Fatalf("the round's child set is %d forms, past max 90 (creates posted %d)", len(set), len(cg.mutations))
	}
}

// gpt r2 B2: a created issue is not ended by a later complete search that
// does not return it (searches find pull requests only).
func TestRunOurIssueSurvivesACompleteSearch(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	fg := issueWorld(t, synthIssue(1001, "ours", "b\n"+praxisstd.Marker(askID)))
	c, ttl := cfg(t, `{"repos":["example/repo"],"rules":[{"repo":"example/repo","author":["someone"]}]}`)
	key := IssueKey("example/repo", 1001)
	st := State{Entities: map[string]*Entity{key: {Kind: KindIssue, Repo: "example/repo", N: 1001, Tier: "hot", Since: t0, Seen: t0}},
		OurIssues: map[string]Ref{key: {Repo: "example/repo", N: 1001, Issue: true}}}
	kids := map[string]string{key: "@000000f1"}
	for i := 0; i < 3; i++ {
		r, err := Run(context.Background(), fg, st, Pass{Config: c, ClosedTTL: ttl, Now: t0.Add(time.Duration(i) * 10 * time.Minute), Children: copyKids(kids), Ours: "ours"})
		if err != nil {
			t.Fatal(err)
		}
		e := r.State.Entities[key]
		if e == nil || !e.EndedAt.IsZero() {
			t.Fatalf("round %d: a complete search ended our open issue: %+v", i, e)
		}
		for _, f := range r.Forms {
			if f.Key == key && f.Retire {
				t.Fatalf("round %d retired our issue's form", i)
			}
		}
		st = r.State
	}
}
