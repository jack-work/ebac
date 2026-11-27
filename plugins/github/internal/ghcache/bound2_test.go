package ghcache

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// busyWorld: n open PRs each with comments comments, the first ours of them ours.
func busyWorld(t *testing.T, n, comments, ours int) (*writeWorld, map[int]*PR) {
	prs := map[int]*PR{}
	var nums []string
	for i := 0; i < n; i++ {
		prs[3000+i] = busyPRBy(t, 3000+i, comments, func(j int) bool { return j < ours })
		nums = append(nums, fmt.Sprint(3000+i))
	}
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{},
		prs: map[string]map[int]*PR{"acme/widget": prs}, wr: &fakeWrites{repoLabels: map[string]bool{"area:core": true}}}
	c, ttl := cfg(t, `{"repos":["acme/widget"],"max":80,"rules":[{"repo":"acme/widget","prs":[`+strings.Join(nums, ",")+`]}]}`)
	w := &writeWorld{t: t, gh: gh, c: c, ttl: ttl, forms: map[string]map[string]json.RawMessage{}, now: t0, ours: "our-author",
		critic: &model.Critic{Name: "ghc", Source: model.SourceGHCache, Write: true, GHWrite: &model.GHWrite{
			Scope: model.GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}}, Fields: model.GHWriteFields}, Maintainer: trustedMaint}}
	return w, prs
}

// #71v part 2 (live 11-26: two busy PRs held no comment hash at all): a round that reads every entity deep
// while no comment moved used to carry each entity's comment hashes twice (base and pend), crossing MaxState and
// cutting the PRs holding the most. A pend whose hashes equal its base's stores none: nothing is cut, every entity
// keeps its hashes. RED on 9229b93: entities cut (over-bound.state).
func TestADeepRoundWithNoCommentMovedCutsNothing(t *testing.T) {
	w, prs := busyWorld(t, 44, 45, 25)
	w.round()
	w.round()
	for _, pr := range prs {
		bump(pr) // CI moved, say: every entity read deep, no comment changed
	}
	w.round()
	t.Logf("the plugin's state after the deep round: %d B", stateBytes(w))
	for k, e := range w.st.Entities {
		if _, ok := e.Alarms["over-bound.state"]; ok {
			t.Fatalf("%s cut (over-bound.state) in a round where no comment moved (%d B)", k, stateBytes(w))
		}
		if e.Base == nil || len(e.Base.Comments) != 45 {
			t.Fatalf("%s: base %v, want its 45 comment hashes", k, e.Base != nil)
		}
	}
	w.round() // the pend lands: the base keeps its hashes
	for k, e := range w.st.Entities {
		if e.Base == nil || len(e.Base.Comments) != 45 {
			t.Fatalf("%s after landing: want its 45 comment hashes kept", k)
		}
	}
}

// #71v part 2: a PR with 62 of our comments (one live PR on 11-26) keeps every one of ours in its base: no over-bound.
// RED on 9229b93: 50 kept, 12 left out with an alarm.
func TestSixtyTwoOfOursAllKept(t *testing.T) {
	r := newCommentRig(t)
	setOurs(r, 60) // 62 of ours with the rig's two
	r.round(func(*GH) {})
	ours := 0
	for id := range r.e.Base.Comments {
		if id != "IC_00000002" {
			ours++
		}
	}
	if ours != 62 {
		t.Fatalf("base keeps %d of our 62 comments", ours)
	}
	if _, ok := r.e.Alarms["over-bound.comments"]; ok {
		t.Fatalf("62 of ours alarmed over-bound: %v", r.e.Alarms)
	}
}

// #71v part 2: a created issue whose form is minted (held) keeps no body or praxis in the cache: what was asked
// lives on its form (22 KB of the live state on 11-26 was six created issues' bodies). RED on 9229b93: kept.
func TestACreatedAndMintedIssueDropsItsBody(t *testing.T) {
	st := State{Creates: map[string]*Create{"p1": {Repo: "acme/widget", State: "created", At: t0, ID: "p1", Issue: 9,
		Key: "widget-issue-9", Title: "t", Body: strings.Repeat("b", 4000), Praxis: json.RawMessage(`{"title":"t"}`)}},
		OurIssues: map[string]Ref{"widget-issue-9": {Repo: "acme/widget", N: 9, Issue: true}}}
	sy := &syncer{now: t0, logf: func(string, ...any) {}}
	c := &creator{s: sy, st: &st, p: Pass{Now: t0, Children: map[string]string{"widget-issue-9": "@f9"}}, reg: map[string]any{}}
	c.issues(map[string]json.RawMessage{"p1": json.RawMessage(`{"title":"t"}`)})
	cr := st.Creates["p1"]
	if cr.Body != "" || len(cr.Praxis) != 0 || cr.State != "created" || cr.Issue != 9 {
		t.Fatalf("minted create keeps %d B of body, %d B of praxis (state %s, issue %d)", len(cr.Body), len(cr.Praxis), cr.State, cr.Issue)
	}
}
