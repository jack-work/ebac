package ghcache

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// #71s (live 11-25): the critic form's state grew past figaro's 196,608 B budget (212,621 B asked at 13:16Z) and
// every round's write was refused. Measured on the form: 79 entities, 35 of them merged; pend 95 KB, nearly all of
// it the comment hash map of each entity's base. Synthesized fixtures only (repo acme/widget, logins
// our-author / reviewer, node ids IC_kwSYNTH... / PRRC_kwSYNTH... of the real ids' length).

// incidentCounts are the comment counts of the 44 open entities as measured (pend.base.comments), reused for the
// 35 merged ones (their first 35).
var incidentCounts = []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 2, 2, 3, 3, 4, 4, 4, 4, 6, 6, 7, 8, 9, 10, 19, 19, 19, 24,
	25, 29, 35, 42, 54, 63, 67, 69, 73, 93, 95, 104, 123, 132}

// busyPR is synthPR with n comments: up to 30 in the conversation (comments(last:30)), the rest three to a review
// thread (comments(last:3)); every fifth by our-author, the rest by reviewer.
func busyPR(t *testing.T, num, n int) *PR {
	return busyPRBy(t, num, n, func(i int) bool { return i%5 == 0 })
}

// busyPRBy is busyPR with comment i by our-author when ours(i).
func busyPRBy(t *testing.T, num, n int, ours func(int) bool) *PR {
	t.Helper()
	pr := synthPR(t, num, "our-author", []string{"area:core"}, false, 0)
	author := func(i int) *Login {
		if ours(i) {
			return &Login{Login: "our-author"}
		}
		return &Login{Login: "reviewer"}
	}
	at := func(i int) string { return fmt.Sprintf("2026-11-%02dT%02d:%02d:00Z", 10+i/600, i/60%10, i%60) }
	conv := min(n, 30)
	for i := 0; i < conv; i++ {
		pr.Comments.Nodes = append(pr.Comments.Nodes, Comment{ID: fmt.Sprintf("IC_kwSYNTH%04d%06d", num, i), Author: author(i),
			Body: fmt.Sprintf("A review note %d on change %d.", i, num), CreatedAt: at(i), UpdatedAt: at(i)})
	}
	pr.Comments.TotalCount = conv
	for i, th := conv, 0; i < n; th++ {
		var cs []Comment
		for j := 0; j < 3 && i < n; j, i = j+1, i+1 {
			cs = append(cs, Comment{ID: fmt.Sprintf("PRRC_kwSYNTH%04d%04d%d", num, th, j), Author: author(i),
				Body: fmt.Sprintf("Thread %d reply %d.", th, j), CreatedAt: at(i), UpdatedAt: at(i)})
		}
		node := pr.Threads.Nodes
		node = append(node, struct {
			ID       string `json:"id"`
			Path     string `json:"path"`
			Resolved bool   `json:"isResolved"`
			Comments struct {
				TotalCount int       `json:"totalCount"`
				Nodes      []Comment `json:"nodes"`
			} `json:"comments"`
		}{ID: fmt.Sprintf("PRRT_kwSYNTH%04d%04d", num, th), Path: fmt.Sprintf("src/f%d.go", th)})
		node[len(node)-1].Comments.TotalCount = len(cs)
		node[len(node)-1].Comments.Nodes = cs
		pr.Threads.Nodes = node
	}
	pr.Threads.TotalCount = len(pr.Threads.Nodes)
	return pr
}

// incidentWorld: 79 explicit PRs of acme/widget with the measured comment counts, minted and agreed over two
// rounds (the first mints, the second lands the base), then 35 of them merged.
func incidentWorld(t *testing.T) *writeWorld {
	prs := map[int]*PR{}
	var nums []string
	for i := 0; i < 79; i++ {
		num := 1000 + i
		prs[num] = busyPR(t, num, incidentCounts[i%len(incidentCounts)])
		nums = append(nums, fmt.Sprint(num))
	}
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{},
		prs: map[string]map[int]*PR{"acme/widget": prs}, wr: &fakeWrites{repoLabels: map[string]bool{"area:core": true}}}
	c, ttl := cfg(t, `{"repos":["acme/widget"],"max":80,"rules":[{"repo":"acme/widget","prs":[`+strings.Join(nums, ",")+`]}]}`)
	w := &writeWorld{t: t, gh: gh, c: c, ttl: ttl, forms: map[string]map[string]json.RawMessage{}, now: t0,
		critic: &model.Critic{Name: "ghc", Source: model.SourceGHCache, Write: true, GHWrite: &model.GHWrite{
			Scope: model.GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}}, Fields: model.GHWriteFields}, Maintainer: trustedMaint}}
	w.round()
	w.round()
	for i := 0; i < 35; i++ {
		pr := prs[1000+44+i%35]
		pr.State, pr.Mergeable, pr.MergeSt = "MERGED", "UNKNOWN", "UNKNOWN"
		bump(pr)
	}
	return w
}

// stateBytes is what the plugin puts on the critic form: runtime.gh_cache and track.
func stateBytes(w *writeWorld) int { return len(mustJSON(w.st)) + len(mustJSON(w.r.Track)) }

// The incident's shape: the round that reads the merges also reads every open PR deep (each moved: CI, a push, a
// comment), so every entity carries a pend beside its base. The state that round asks to write, the plugin's part
// of the form, is under three quarters of the 196,608 B budget (147,456 B); ended entities carry no write path,
// open ones keep their base.
func TestTheIncidentShapedStateStaysUnderThreeQuartersOfTheBudget(t *testing.T) {
	w := incidentWorld(t)
	for n := 1000; n < 1044; n++ {
		bump(w.gh.prs["acme/widget"][n])
	}
	w.round() // reads all 79 deep: the 35 end
	got := stateBytes(w)
	t.Logf("the plugin's state: %d B", got)
	if got >= 147456 {
		t.Fatalf("the plugin's state is %d B, want < 147456 (75%% of the 196608 B budget)", got)
	}
	ended, open := 0, 0
	for k, e := range w.st.Entities {
		switch {
		case !e.EndedAt.IsZero():
			ended++
			if e.Base != nil || e.Pend != nil || e.RO != "" {
				t.Fatalf("%s ended but keeps its write path: base %v pend %v ro %q", k, e.Base != nil, e.Pend != nil, e.RO)
			}
		default:
			open++
			if e.Base == nil {
				t.Fatalf("%s is open but has no base", k)
			}
		}
	}
	if ended != 35 || open != 44 {
		t.Fatalf("ended %d open %d, want 35 and 44", ended, open)
	}
}

// An ended entity with a form and no base is not read deep every round for a base (it would cost a deep read per
// round for ClosedTTL); a mark that does not move reads nothing.
func TestAnEndedEntityIsNotReadDeepForABase(t *testing.T) {
	w := incidentWorld(t)
	w.round()
	w.round()
	before := len(w.gh.calls)
	w.round()
	for _, q := range w.gh.calls[before:] {
		if strings.Contains(q, "...D") {
			t.Fatalf("a quiet round read deep: %.200s", q)
		}
	}
	if len(w.r.Telemetry.Deep) != 0 {
		t.Fatalf("deep reads in a quiet round: %v", w.r.Telemetry.Deep)
	}
}

// Back from its end (reopened), an entity is read deep in its next round and agrees a base again.
func TestAReopenedEntityAgreesABaseAgain(t *testing.T) {
	w := incidentWorld(t)
	w.round()
	w.round()
	pr := w.gh.prs["acme/widget"][1044]
	k := keyOf(KindPR, "acme/widget", 1044)
	if e := w.st.Entities[k]; e == nil || e.EndedAt.IsZero() || e.Base != nil {
		t.Fatalf("1044 should be ended and shed: %+v", e)
	}
	pr.State, pr.Mergeable, pr.MergeSt = "OPEN", "MERGEABLE", "BLOCKED"
	bump(pr)
	w.now = w.now.Add(ColdEvery) // an ended entity's marks are read every ColdEvery
	w.round()                    // the mark moves: read deep, the end clears, a pend is agreed
	w.round()                    // the pend lands: the base
	e := w.st.Entities[k]
	if e == nil || !e.EndedAt.IsZero() || e.Base == nil {
		t.Fatalf("reopened 1044: %+v", e)
	}
}

// The per-entity bound: past 20 comments a base keeps every one of ours (up to 50) and the 20 newest of the others;
// someone's past the bound raise no alarm.
func TestABaseKeepsOursAndTwentyOfTheOthers(t *testing.T) {
	r := newCommentRig(t)
	for i := 10; i < 40; i++ { // 30 of someone's
		r.gh.comments[fmt.Sprintf("IC_000000%02d", i)] = &fakeComment{body: "noted", author: "someone", number: 7}
	}
	r.round(func(*GH) {})
	if n := len(r.e.Base.Comments); n != 22 {
		t.Fatalf("base keeps %d comment hashes, want 22 (our 2 and 20 of someone's)", n)
	}
	for _, id := range []string{"IC_00000001", "PRRC_00000003"} {
		if _, ok := r.e.Base.Comments[id]; !ok {
			t.Fatalf("our %s fell out of the base: %v", id, r.e.Base.Comments)
		}
	}
	if a := r.alarm("over-bound", "comments"); a != nil {
		t.Fatalf("only someone's past the bound, yet alarmed: %+v", a)
	}
}

// Ours past 50 raise one over-bound alarm (and its wake) instead of growing the base; back under for two reads, it
// clears.
func TestOursPastTheBoundRaiseOneOverBoundAlarm(t *testing.T) {
	r := newCommentRig(t)
	extra := MaxBaseOurs + 3 // with the rig's two: MaxBaseOurs+5 of ours
	for i := 0; i < extra; i++ {
		r.gh.comments[fmt.Sprintf("IC_9%07d", i)] = &fakeComment{body: "mine", author: "ours", number: 7}
	}
	r.round(func(*GH) {})
	if n := len(r.e.Base.Comments); n != MaxBaseOurs+1 {
		t.Fatalf("base keeps %d comment hashes, want %d (%d of ours and someone's one)", n, MaxBaseOurs+1, MaxBaseOurs)
	}
	a := r.e.Alarms["over-bound.comments"]
	if want := fmt.Sprintf("5 of our comments are past the base bound (the newest %d of ours are kept): an edit of those is not written", MaxBaseOurs); a.Detail != want {
		t.Fatalf("over-bound alarm = %+v (alarms %v)", a, r.e.Alarms)
	}
	for i := 0; i < extra; i++ {
		delete(r.gh.comments, fmt.Sprintf("IC_9%07d", i))
	}
	r.round(func(*GH) {})
	r.round(func(*GH) {}) // two clean reads in a row leave the bound (#71v)
	if _, ok := r.e.Alarms["over-bound.comments"]; ok {
		t.Fatalf("back under the bound, the alarm stands: %v", r.e.Alarms)
	}
}

// boundBase alone: past the bound the newest 20 of the others stay (by created_at), ours whatever their age; a
// standing over-bound alarm clears once none of ours is left out.
func TestTheBoundKeepsTheNewestOfTheOthersAndClearsItsAlarm(t *testing.T) {
	g := GH{Comments: map[string]CachedComment{}}
	b := Base{Comments: map[string]string{}}
	for i := 0; i < 24; i++ { // someone's, created one minute apart: IC_00000000 the oldest
		id := fmt.Sprintf("IC_%08d", i)
		g.Comments[id] = CachedComment{Author: "someone", Body: "x", CreatedAt: fmt.Sprintf("2026-11-20T04:%02d:00Z", i)}
		b.Comments[id] = "h"
	}
	g.Comments["IC_OURS0001"] = CachedComment{Author: "Ours", Body: "x", CreatedAt: "2026-11-01T00:00:00Z"} // the oldest of all
	b.Comments["IC_OURS0001"] = "h"
	e := &Entity{Alarms: map[string]Alarm{"over-bound.comments": {Detail: "an earlier round"}}}
	s := &syncer{p: Pass{Ours: "ours"}, now: t0, logf: func(string, ...any) {}}
	s.boundBase("k", e, &b, &g)
	var kept []string
	for id := range b.Comments {
		kept = append(kept, id)
	}
	slices.Sort(kept)
	want := []string{"IC_00000004", "IC_00000005", "IC_00000006", "IC_00000007", "IC_00000008", "IC_00000009", "IC_00000010", "IC_00000011",
		"IC_00000012", "IC_00000013", "IC_00000014", "IC_00000015", "IC_00000016", "IC_00000017", "IC_00000018", "IC_00000019",
		"IC_00000020", "IC_00000021", "IC_00000022", "IC_00000023", "IC_OURS0001"}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	if _, ok := e.Alarms["over-bound.comments"]; ok {
		t.Fatalf("none of ours left out, yet the alarm stands: %v", e.Alarms)
	}
}

// The first base of an entity (no base yet: the form is rewritten from GitHub) is bounded the same way.
func TestAFirstBaseIsBoundedToo(t *testing.T) {
	r := newCommentRig(t)
	for i := 10; i < 40; i++ {
		r.gh.comments[fmt.Sprintf("IC_000000%02d", i)] = &fakeComment{body: "noted", author: "someone", number: 7}
	}
	r.e.Base = nil
	r.round(func(*GH) {})
	if r.e.Base == nil || len(r.e.Base.Comments) != 22 {
		t.Fatalf("first base: %+v, want 22 comment hashes (our 2 and 20 of someone's)", r.e.Base)
	}
}

// GPT review r1 (BLOCK): the per-entity bounds admit 44 open PRs each with exactly 50 of ours and 20 others'
// comments, all read deep in one round: 70 hashes in each base and pend, 246 KB of maps alone. The state bound
// drops the comment hashes of the entities holding the most, with an over-bound.state alarm on each, until the
// plugin's part of the form is at most MaxState (131,072 B); a round that fits clears those alarms.
func TestTheWorstCaseThePerEntityBoundsAdmitIsCutToTheStateBound(t *testing.T) {
	w, prs := worstCaseWorld(t)
	w.round()
	w.round()
	for _, pr := range prs {
		bump(pr)
	}
	w.round() // every entity read deep: a base and a pend each
	got := stateBytes(w)
	t.Logf("the plugin's state: %d B", got)
	if got > 131072 {
		t.Fatalf("the plugin's state is %d B, want <= 131072 (MaxState)", got)
	}
	cut := 0
	for _, e := range w.st.Entities {
		if _, ok := e.Alarms["over-bound.state"]; ok {
			cut++
			if (e.Base != nil && len(e.Base.Comments) != 0) || (e.Pend != nil && len(e.Pend.Base.Comments) != 0) {
				t.Fatalf("an entity alarmed over-bound.state keeps comment hashes")
			}
		}
	}
	if cut == 0 || cut == 44 {
		t.Fatalf("%d of 44 entities cut, want some but not all", cut)
	}
	for n := 2000; n < 2034; n++ { // 34 close and are shed: the rest fit
		prs[n].State = "CLOSED"
		bump(prs[n])
	}
	w.round()
	w.round()
	for n := 2034; n < 2044; n++ { // the rest are read deep again: their hashes return, and fit
		bump(prs[n])
	}
	w.round()
	w.round()
	for k, e := range w.st.Entities {
		if _, ok := e.Alarms["over-bound.state"]; ok {
			t.Fatalf("%s: the state fits, yet over-bound.state stands (%d B)", k, stateBytes(w))
		}
	}
}

// worstCaseWorld: 44 open PRs each with exactly MaxBaseOurs of ours and MaxBaseOthers others' comments (GPT review r1, #71s).
func worstCaseWorld(t *testing.T) (*writeWorld, map[int]*PR) {
	prs := map[int]*PR{}
	var nums []string
	for i := 0; i < 44; i++ {
		prs[2000+i] = busyPRBy(t, 2000+i, MaxBaseOurs+MaxBaseOthers, func(j int) bool { return j < MaxBaseOurs })
		nums = append(nums, fmt.Sprint(2000+i))
	}
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{},
		prs: map[string]map[int]*PR{"acme/widget": prs}, wr: &fakeWrites{repoLabels: map[string]bool{"area:core": true}}}
	c, ttl := cfg(t, `{"repos":["acme/widget"],"max":80,"rules":[{"repo":"acme/widget","prs":[`+strings.Join(nums, ",")+`]}]}`)
	w := &writeWorld{t: t, gh: gh, c: c, ttl: ttl, forms: map[string]map[string]json.RawMessage{}, now: t0, ours: "our-author",
		critic: &model.Critic{Name: "ghc", Source: model.SourceGHCache, Write: true, GHWrite: &model.GHWrite{
			Scope: model.GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}}, Fields: model.GHWriteFields}, Maintainer: trustedMaint}}
	return w, prs
}
