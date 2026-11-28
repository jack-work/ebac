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
	"github.com/gluck/ebac/sdk/plugin"
)

// #71w: creates stalled at the form cap (11-26 03Z, 11-27 21-22Z, 11-28 02Z):
// the cap filled with our own created issues and live PRs; a waiting create
// never evicted an ended form, never outranked a new PR, paid a listing every
// round and raised nothing.

const capRepo = "acme/widget"

type capCase struct {
	live, ended, fresh []int // PRs: held and open, held and merged (in their grace), open and not yet tracked
	ours               []int // our created issues, held and open
	creates            int   // creates recorded "creating" (they post this round)
	max, reserve       int
	ttl                time.Duration
	endedAt            time.Time // when the ended PRs were first seen merged
}

type capWorld struct {
	gh    comboGH
	cg    *createGH
	st    State
	kids  map[string]string
	asks  map[string]json.RawMessage
	cfg   *Config
	ttl   time.Duration
	order []string       // the ended keys, longest-ended first
	reg   map[string]any // the register's sync key as last written (a round writes it only when it changes)
	regW  int            // rounds that wrote the register
}

func newCapWorld(t *testing.T, cc capCase, now time.Time) *capWorld {
	t.Helper()
	prs := map[int]*PR{}
	open := []int{}
	for _, n := range append(append([]int{}, cc.live...), cc.fresh...) {
		prs[n] = synthPR(t, n, "our-author", nil, false, 0)
		open = append(open, n)
	}
	for _, n := range cc.ended {
		pr := synthPR(t, n, "our-author", nil, false, 0)
		pr.State = "MERGED"
		prs[n] = pr
	}
	fg := &fakeGH{t: t, failLand: -1, children: map[string]string{}, prs: map[string]map[int]*PR{capRepo: prs},
		search: map[string][]int{"repo:" + capRepo + " is:pr is:open author:our-author": open}}
	cg := &createGH{now: now}
	w := &capWorld{gh: comboGH{fakeGH: fg, c: cg}, cg: cg, kids: map[string]string{}, asks: map[string]json.RawMessage{}, ttl: cc.ttl}
	raw := fmt.Sprintf(`{"repos":[%q],"max":%d,"reserve_ours":%d,"rules":[{"repo":%q,"author":["our-author"]}],"ttl":{"closed":%q}}`,
		capRepo, cc.max, cc.reserve, capRepo, cc.ttl.String())
	w.cfg, w.ttl = cfg(t, raw)
	w.st = State{Entities: map[string]*Entity{}, Creates: map[string]*Create{}, OurIssues: map[string]Ref{}}
	recent := now.Add(-time.Minute) // marks just read: nothing is due but what this round searches
	slot := 0
	hold := func(k string, e *Entity) {
		slot++
		w.kids[k] = fmt.Sprintf("@%08d", slot)
		e.Digest, e.MarkAt, e.Since, e.Seen, e.Tier = "d", recent, now.Add(-48*time.Hour), recent, "hot"
		w.st.Entities[k] = e
	}
	for _, n := range cc.live {
		hold(ChildKey(capRepo, n), &Entity{Repo: capRepo, N: n, State: "OPEN"})
	}
	for i, n := range cc.ended {
		k := ChildKey(capRepo, n)
		// The first ended PR ended earliest: it is the one a create evicts first.
		hold(k, &Entity{Repo: capRepo, N: n, State: "MERGED", EndedAt: cc.endedAt.Add(time.Duration(i) * time.Minute)})
		w.order = append(w.order, k)
	}
	for _, n := range cc.ours {
		k := IssueKey(capRepo, n)
		hold(k, &Entity{Kind: KindIssue, Repo: capRepo, N: n, State: "OPEN"})
		w.st.OurIssues[k] = Ref{Repo: capRepo, N: n, Issue: true}
	}
	for i := 1; i <= cc.creates; i++ {
		id := fmt.Sprintf("%012d", i)
		raw := askIn(capRepo, id, nil)
		w.asks[id] = raw
		w.st.Creates[id] = &Create{Repo: capRepo, State: "creating", At: now.Add(-time.Hour), Req: hashJSON(raw), Title: "t",
			Body: "b\n" + praxisstd.Marker(id), Praxis: raw}
	}
	return w
}

// run is one round as the core runs it: wake set is a write-only round.
func (w *capWorld) run(t *testing.T, now time.Time, wake bool) Result {
	t.Helper()
	p := Pass{Config: w.cfg, ClosedTTL: w.ttl, Now: now, Auth: authFor(capRepo), Ours: "ours", Children: copyKids(w.kids), PraxisNew: w.asks}
	if wake {
		p.Wake = map[string]bool{"@register": true}
	}
	r, err := Run(context.Background(), w.gh, w.st, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := poll.ValidateForms(ghcCritic(copyKids(w.kids)), &plugin.Reply{Forms: r.Forms}); err != nil {
		t.Fatalf("the core refuses the round's reply: %v", err)
	}
	for _, f := range r.Forms { // the core lands every write
		if f.Retire {
			delete(w.kids, f.Key)
		} else if _, ok := w.kids[f.Key]; !ok {
			w.kids[f.Key] = "@new-" + f.Key
		}
	}
	if len(w.kids) > maxForms(w.cfg) {
		t.Fatalf("the child set is %d forms, past max %d", len(w.kids), maxForms(w.cfg))
	}
	w.st = r.State
	if r.Register != nil {
		w.reg, w.regW = r.Register, w.regW+1
	}
	return r
}

func retires(r Result) []string {
	var out []string
	for _, f := range r.Forms {
		if f.Retire {
			out = append(out, f.Key)
		}
	}
	return out
}

func waitingAlarm(reg map[string]any, id string) any {
	al, _ := reg["alarm"].(map[string]any)
	cw, _ := al["create-waiting"].(map[string]any)
	return cw[id]
}

func seq(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

// A cap full of ENDED forms: the create takes the longest-ended form's slot in
// that round, a write-only round too (eviction needs no read).
func TestCapAWaitingCreateEvictsTheLongestEndedForm(t *testing.T) {
	for _, wake := range []bool{false, true} {
		t.Run(fmt.Sprintf("write-only=%v", wake), func(t *testing.T) {
			now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
			w := newCapWorld(t, capCase{live: seq(1, 55), ended: seq(100, 5), ours: seq(500, 30), creates: 1, max: 90, reserve: 30,
				ttl: 6 * time.Hour, endedAt: now.Add(-time.Hour)}, now)
			r := w.run(t, now, wake)
			if len(w.cg.mutations) != 1 {
				t.Fatalf("creates posted %d (want 1): log %v", len(w.cg.mutations), r.Log)
			}
			if got := retires(r); len(got) != 1 || got[0] != w.order[0] {
				t.Fatalf("retires %v (want only the longest-ended %s)", got, w.order[0])
			}
		})
	}
}

// A cap full of LIVE forms: the create waits, pays no listing while no slot
// can be had, and raises one create-waiting alarm from its second round on
// (the same value every round: the register is not rewritten for it).
func TestCapAFullCapOfLiveFormsAlarmsOnceAndSpendsNothing(t *testing.T) {
	now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	w := newCapWorld(t, capCase{live: seq(1, 60), ours: seq(500, 30), creates: 1, max: 90, reserve: 30, ttl: 6 * time.Hour}, now)
	var alarms []string
	var wrote []int
	for i := range 3 {
		r := w.run(t, now.Add(time.Duration(i)*10*time.Second), true)
		alarms = append(alarms, fmt.Sprint(waitingAlarm(w.reg, "000000000001")))
		wrote = append(wrote, w.regW)
		if len(retires(r)) != 0 {
			t.Fatalf("round %d retired %v: no live form yields to a create", i+1, retires(r))
		}
	}
	if len(w.cg.mutations) != 0 || w.cg.reads != 0 {
		t.Fatalf("with no slot the create posted %d and listed %d time(s) (want 0 and 0)", len(w.cg.mutations), w.cg.reads)
	}
	if alarms[0] != "<nil>" || alarms[1] == "<nil>" || alarms[2] != alarms[1] || wrote[2] != wrote[1] {
		t.Fatalf("create-waiting alarm by round: %q, register writes %v (want none, then one value, not written again)", alarms, wrote)
	}
	if !strings.Contains(alarms[1], "90") {
		t.Fatalf("the alarm does not name the cap: %s", alarms[1])
	}
}

// A create outranks a NEW PR: with one free slot, the create takes it and the
// new PRs are not admitted (logged), the cap and the round's reply hold.
func TestCapACreateOutranksANewPR(t *testing.T) {
	now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	w := newCapWorld(t, capCase{live: seq(1, 59), fresh: seq(200, 2), ours: seq(500, 30), creates: 1, max: 90, reserve: 30, ttl: 6 * time.Hour}, now)
	r := w.run(t, now, false)
	if len(w.cg.mutations) != 1 {
		t.Fatalf("creates posted %d (want 1): log %v", len(w.cg.mutations), r.Log)
	}
	for _, n := range []int{200, 201} {
		if _, ok := w.kids[ChildKey(capRepo, n)]; ok {
			t.Fatalf("new PR %d took a slot from the create", n)
		}
	}
	if !strings.Contains(strings.Join(r.Log, "\n"), "not tracked") {
		t.Fatalf("the PRs left out are not logged: %v", r.Log)
	}
}

// PR tracking stops at max - reserve_ours: the reserve stays for our issues.
func TestCapPRTrackingLeavesTheReserve(t *testing.T) {
	now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	w := newCapWorld(t, capCase{live: seq(1, 50), fresh: seq(200, 20), ours: seq(500, 5), max: 90, reserve: 30, ttl: 6 * time.Hour}, now)
	w.run(t, now, false)
	prs := 0
	for k := range w.kids {
		if strings.Contains(k, "-pr-") {
			prs++
		}
	}
	if prs != 60 {
		t.Fatalf("%d PR forms after admission (want max 90 - reserve 30 = 60)", prs)
	}
}

// ttl.closed is honoured across write-only rounds: a merged PR and a closed
// issue of ours retire at the first full round past it, never before.
func TestCapEndedFormsRetireAtTTLAcrossWriteOnlyRounds(t *testing.T) {
	now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	w := newCapWorld(t, capCase{live: seq(1, 3), ended: []int{100}, ours: []int{500}, max: 90, reserve: 30, ttl: time.Hour, endedAt: now}, now)
	w.st.Entities[IssueKey(capRepo, 500)].State, w.st.Entities[IssueKey(capRepo, 500)].EndedAt = "CLOSED", now
	at := now
	for i := range 5 { // write-only rounds through the hour
		at = now.Add(time.Duration(i+1) * 13 * time.Minute)
		w.run(t, at, true)
	}
	if _, ok := w.kids[ChildKey(capRepo, 100)]; !ok {
		t.Fatal("a write-only round retired an ended form before its ttl was judged by a full round")
	}
	r := w.run(t, now.Add(time.Hour+time.Minute), false)
	got := strings.Join(retires(r), ",")
	if !strings.Contains(got, ChildKey(capRepo, 100)) || !strings.Contains(got, IssueKey(capRepo, 500)) {
		t.Fatalf("past ttl.closed the full round retired %q (want the merged PR and the closed issue of ours)", got)
	}
	if _, ok := w.st.OurIssues[IssueKey(capRepo, 500)]; ok {
		t.Fatal("a retired issue of ours is still ours (it would be enrolled again)")
	}
}

func TestCapReserveOursIsBounded(t *testing.T) {
	for _, raw := range []string{`{"repos":["a/b"],"max":40,"reserve_ours":41,"rules":[{"author":["x"]}]}`,
		`{"repos":["a/b"],"reserve_ours":-1,"rules":[{"author":["x"]}]}`} {
		if _, _, err := ParseConfig(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	c, _ := cfg(t, `{"repos":["a/b"],"max":90,"rules":[{"author":["x"]}]}`)
	if reserveOurs(c) != DefaultReserveOurs {
		t.Fatalf("unset reserve_ours is %d (want %d)", reserveOurs(c), DefaultReserveOurs)
	}
}
