package ghcache

import (
	"fmt"
	"strings"
	"testing"
)

// overPings counts this rig's maintainer pings naming key, raised and cleared apart.
func overPings(r *commentRig, key string) (raised, cleared int) {
	for _, p := range r.pings {
		if !strings.Contains(p, key) {
			continue
		}
		if strings.Contains(p, "cleared") {
			cleared++
		} else {
			raised++
		}
	}
	return raised, cleared
}

// setOurs makes n of the rig's extra comments ours (IC_00000010 on), the rest gone.
func setOurs(r *commentRig, n int) {
	for i := 10; i < 70; i++ {
		id := fmt.Sprintf("IC_000000%02d", i)
		if i-10 < n {
			r.gh.comments[id] = &fakeComment{body: "mine", author: "ours", number: 7}
		} else {
			delete(r.gh.comments, id)
		}
	}
}

// #71v: an entity whose reads alternate over and under the ours bound (a long thread read whole one round, its
// newest only the next) raises over-bound.comments ONCE, one wake, not once per read. Two clean reads in a row
// leave it: the alarm clears with one "cleared" ping. A new episode raises again. RED on 723de8f4: a raise per
// over read.
func TestOverBoundCommentsRaisesOnceWhileItFlaps(t *testing.T) {
	r := newCommentRig(t)
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			setOurs(r, 53) // 55 of ours: past 50
		} else {
			setOurs(r, 43) // 45: under
		}
		r.round(func(*GH) {})
	}
	if raised, cleared := overPings(r, "over-bound.comments"); raised != 1 || cleared != 0 {
		t.Fatalf("10 flapping reads: %d raise ping(s), %d cleared, want 1 and 0 (pings %v)", raised, cleared, r.pings)
	}
	if r.e.Alarms["over-bound.comments"].Detail == "" {
		t.Fatalf("still flapping, the alarm should stand: %v", r.e.Alarms)
	}
	setOurs(r, 43)
	r.round(func(*GH) {}) // the flap's last read was under: this is the second clean read in a row
	r.round(func(*GH) {})
	if raised, cleared := overPings(r, "over-bound.comments"); raised != 1 || cleared != 1 {
		t.Fatalf("after clean reads: %d raise(s), %d cleared, want 1 and 1 (pings %v)", raised, cleared, r.pings)
	}
	if _, ok := r.e.Alarms["over-bound.comments"]; ok {
		t.Fatalf("left the bound, the alarm stands: %v", r.e.Alarms)
	}
	setOurs(r, 53)
	r.round(func(*GH) {})
	if raised, _ := overPings(r, "over-bound.comments"); raised != 2 {
		t.Fatalf("a new episode: %d raise(s), want 2 (pings %v)", raised, r.pings)
	}
}

// #71v (#71u item 2): a seat's ack clears the alarm; while the entity stays over it is not raised again (no wake,
// no alarm back on the form). RED on 723de8f4: the next over read raises it again.
func TestAnAckedOverBoundIsNotRaisedAgainWhileStillOver(t *testing.T) {
	r := newCommentRig(t)
	setOurs(r, 53)
	r.round(func(*GH) {})
	delete(r.e.Alarms, "over-bound.comments") // the ack (ghcache.go 4b)
	for i := 0; i < 3; i++ {
		r.round(func(*GH) {})
	}
	if raised, _ := overPings(r, "over-bound.comments"); raised != 1 {
		t.Fatalf("acked while over: %d raise ping(s), want 1 (pings %v)", raised, r.pings)
	}
	if _, ok := r.e.Alarms["over-bound.comments"]; ok {
		t.Fatalf("acked while over, the alarm is back: %v", r.e.Alarms)
	}
}

// #71v (#71u item 6): a round that fits only because an entity's comment hashes were dropped does not clear its
// over-bound.state: the hashes are still gone, so an edit of its comments is still not written. RED on 723de8f4:
// the fitting round clears it.
func TestAFittingRoundKeepsOverBoundStateUntilTheHashesReturn(t *testing.T) {
	w, prs := worstCaseWorld(t)
	w.round()
	w.round()
	for _, pr := range prs {
		bump(pr)
	}
	w.round() // the cut
	var cut []string
	for k, e := range w.st.Entities {
		if _, ok := e.Alarms["over-bound.state"]; ok {
			cut = append(cut, k)
		}
	}
	if len(cut) == 0 {
		t.Fatalf("no entity cut")
	}
	r := w.round() // nothing moves: the state fits, the cut entities' hashes are still dropped
	for _, k := range cut {
		e := w.st.Entities[k]
		if _, ok := e.Alarms["over-bound.state"]; !ok {
			t.Fatalf("%s: its hashes are still dropped (base %v), yet over-bound.state cleared", k, e.Base != nil && len(e.Base.Comments) > 0)
		}
	}
	for _, p := range r.Pings {
		if strings.Contains(p.Text, "over-bound.state") {
			t.Fatalf("a fitting round with nothing read pinged: %q", p.Text)
		}
	}
}
