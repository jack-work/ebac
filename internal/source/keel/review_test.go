package keel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

func allStamped(string, Verdict) bool { return true }

func TestUnstampedVerdictsCountAsMissing(t *testing.T) {
	cfg := ReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer"}}
	v := map[string]Verdict{"reviewer": {Verdict: "pass", Rev: "r2"}}
	got := reviewState(cfg, true, v, nil, "r2", func(string, Verdict) bool { return false })
	if got.State != "waiting" || !reflect.DeepEqual(got.Unstamped, []string{"reviewer"}) {
		t.Fatalf("%+v", got)
	}
}

func TestReviewStateArms(t *testing.T) {
	cfg := ReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer", "warden"}}
	open := map[string]IndexEntry{"a": {Path: "d.md"}, "b": {Path: "d.md", Resolved: true}, "c": {Path: "other.md"}}
	closed := map[string]IndexEntry{"b": {Path: "d.md", Resolved: true}}
	pass := Verdict{Verdict: "pass", Rev: "r2"}
	cases := []struct {
		name     string
		verdicts map[string]Verdict
		index    map[string]IndexEntry
		rev      string
		want     ReviewState
	}{
		{"all pass, nothing open", map[string]Verdict{"reviewer": pass, "warden": pass}, closed, "r2",
			ReviewState{State: "pass", Doc: "d.md", Rev: "r2", RevChecked: true}},
		{"all pass but a thread open", map[string]Verdict{"reviewer": pass, "warden": pass}, open, "r2",
			ReviewState{State: "waiting", Doc: "d.md", Rev: "r2", RevChecked: true, Open: 1}},
		{"one missing", map[string]Verdict{"reviewer": pass}, closed, "r2",
			ReviewState{State: "waiting", Doc: "d.md", Rev: "r2", RevChecked: true, Missing: []string{"warden"}}},
		{"one stale", map[string]Verdict{"reviewer": pass, "warden": {Verdict: "pass", Rev: "r1"}}, closed, "r2",
			ReviewState{State: "waiting", Doc: "d.md", Rev: "r2", RevChecked: true, Stale: []string{"warden"}}},
		{"changes wins", map[string]Verdict{"reviewer": {Verdict: "changes", Rev: "r2"}, "warden": {Verdict: "comment", Rev: "r2"}}, closed, "r2",
			ReviewState{State: "changes", Doc: "d.md", Rev: "r2", RevChecked: true, ChangesBy: []string{"reviewer"}, Pending: []string{"warden"}}},
		{"stale changes do not count", map[string]Verdict{"reviewer": {Verdict: "changes", Rev: "r1"}, "warden": pass}, closed, "r2",
			ReviewState{State: "waiting", Doc: "d.md", Rev: "r2", RevChecked: true, Stale: []string{"reviewer"}}},
		{"invalid verdict", map[string]Verdict{"reviewer": {Verdict: "lgtm", Rev: "r2"}, "warden": pass}, closed, "r2",
			ReviewState{State: "waiting", Doc: "d.md", Rev: "r2", RevChecked: true, Invalid: []string{"reviewer"}}},
		{"rev unknown fails closed", map[string]Verdict{"reviewer": pass, "warden": pass}, closed, "",
			ReviewState{State: "waiting", Doc: "d.md"}},
	}
	for _, c := range cases {
		if got := reviewState(cfg, true, c.verdicts, c.index, c.rev, allStamped); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	// No reviewers named: never pass, whatever verdicts are present (the pool is never derived from them).
	if got := reviewState(ReviewConfig{Doc: "d.md"}, true, map[string]Verdict{"reviewer": pass}, closed, "r2", allStamped); got.State != "waiting" {
		t.Errorf("implicit pool passed: %+v", got)
	}
	// An unstamped review key never passes.
	if got := reviewState(cfg, false, map[string]Verdict{"reviewer": pass, "warden": pass}, closed, "r2", allStamped); got.State != "waiting" || !got.ConfigUnstamped {
		t.Errorf("unstamped config: %+v", got)
	}
	if got := reviewState(ReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer"}}, true, map[string]Verdict{"reviewer": pass}, closed, "", allStamped); got.State != "waiting" || got.RevChecked {
		t.Errorf("pass verdict, unknown rev: %+v, want waiting", got)
	}
	if got := reviewState(ReviewConfig{Doc: "d.md"}, true, nil, closed, "r2", allStamped); got.State != "waiting" {
		t.Errorf("no verdicts: %+v", got)
	}
}

func TestReadReviewParsesAndRefuses(t *testing.T) {
	keys := map[string]json.RawMessage{
		ReviewKey:  json.RawMessage(`{"doc":"d.md","reviewers":["reviewer"]}`),
		VerdictKey: json.RawMessage(`{"reviewer":{"verdict":"pass","rev":"r2","at":"2026-11-19T20:00:00Z"},"warden":"nonsense"}`),
	}
	cfg, v, ok, err := readReview(keys)
	if err != nil || !ok || cfg.Doc != "d.md" || v["reviewer"].Rev != "r2" || v["warden"].Verdict != "unreadable" {
		t.Fatalf("cfg %+v v %+v ok %v err %v", cfg, v, ok, err)
	}
	if _, _, ok, err := readReview(map[string]json.RawMessage{}); ok || err != nil {
		t.Fatalf("absent: ok %v err %v", ok, err)
	}
	if _, _, ok, err := readReview(map[string]json.RawMessage{ReviewKey: json.RawMessage(`{"reviewers":["x"]}`)}); ok || err == nil {
		t.Fatalf("no doc: ok %v err %v", ok, err)
	}
}

// logDoor answers keel's log for one path and records the query.
type logDoor struct{ got *string }

func (d logDoor) Where() string { return "log" }
func (d logDoor) Get(_ context.Context, path string, _ time.Duration) (int, []byte, error) {
	*d.got = path
	return 200, []byte(`[{"sha":"a1b2c3","parents":[],"author":"jack","time":1,"subject":"edit d.md"}]`), nil
}

func TestDocRevAsksKeelsLogForThePathOnMain(t *testing.T) {
	var got string
	rev, err := DocRev(context.Background(), &Client{T: logDoor{&got}}, "notes/captains-log", "2026/11/19/d.md")
	if err != nil || rev != "a1b2c3" {
		t.Fatalf("rev %q err %v", rev, err)
	}
	if want := "/api/r/notes-captains-log/log?n=1&path=2026%2F11%2F19%2Fd.md&rev=main"; got != want {
		t.Fatalf("query %q, want %q", got, want)
	}
}

// roundKeys is a round's keys: a review of d.md (reviewer: reviewer), the
// verdicts, the core's check, and the review_state now on the form.
func roundKeys(config bool, valid bool, onForm json.RawMessage) map[string]json.RawMessage {
	cfg := ReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer"}, AuthorRole: "knight"}
	v := Verdict{Verdict: "pass", Rev: "r9"}
	chk := model.KeelReviewCheck{Config: config, Roles: map[string]model.KeelRoleCheck{"reviewer": {Valid: valid}}}
	keys := map[string]json.RawMessage{
		ReviewKey:  model.MustJSON(cfg),
		VerdictKey: model.MustJSON(map[string]Verdict{"reviewer": v}),
		CheckKey:   model.MustJSON(chk),
	}
	if onForm != nil {
		keys[ReviewStateKey] = onForm
	}
	return keys
}

func roundState(t *testing.T, keys map[string]json.RawMessage, byPath map[string][]Thread, cl *Client) (ReviewState, bool) {
	t.Helper()
	rep := &plugin.Reply{}
	w := &model.Critic{Name: "k", FormID: "@7d99311ef30a41de02b53090bd27a2e8", Mode: model.ModeKeel, Keel: &model.KeelRef{Repo: repo}}
	reviewRound(context.Background(), cl, rep, w, &model.KeelState{}, keys, byPath)
	raw, set := rep.Set[ReviewStateKey]
	var rs ReviewState
	_ = json.Unmarshal(raw, &rs)
	return rs, set
}

func stubRev(t *testing.T, rev string, err error) {
	t.Helper()
	old := DocRev
	DocRev = func(context.Context, *Client, string, string) (string, error) { return rev, err }
	t.Cleanup(func() { DocRev = old })
}

var closedDoc = map[string][]Thread{"d.md": {{ID: "a", Resolved: true}}, "other.md": {{ID: "b"}}}

func TestReviewRoundPassesOnlyOnTheCoresCheckAndFreshFacts(t *testing.T) {
	stubRev(t, "r9", nil)
	if rs, _ := roundState(t, roundKeys(true, true, nil), closedDoc, nil); rs.State != "pass" || !rs.RevChecked {
		t.Fatalf("all good: %+v", rs)
	}
	if rs, _ := roundState(t, roundKeys(true, false, nil), closedDoc, nil); rs.State != "waiting" || len(rs.Unstamped) != 1 {
		t.Errorf("core says the verdict does not count: %+v", rs)
	}
	if rs, _ := roundState(t, roundKeys(false, true, nil), closedDoc, nil); rs.State != "waiting" || !rs.ConfigUnstamped {
		t.Errorf("core says the review key is unstamped: %+v", rs)
	}
	keys := roundKeys(true, true, nil)
	delete(keys, CheckKey)
	if rs, _ := roundState(t, keys, closedDoc, nil); rs.State != "waiting" {
		t.Errorf("no core check: %+v", rs)
	}
	open := map[string][]Thread{"d.md": {{ID: "a"}}}
	if rs, _ := roundState(t, roundKeys(true, true, nil), open, nil); rs.State != "waiting" || rs.Open != 1 {
		t.Errorf("an open thread: %+v", rs)
	}
}

// F3/F4: nothing is carried from earlier rounds. No threads this round and
// keel unreadable: waiting; the rev read failing: waiting, rev_checked false.
func TestReviewRoundFailsClosedOnUnreadableFacts(t *testing.T) {
	stubRev(t, "r9", nil)
	if rs, _ := roundState(t, roundKeys(true, true, nil), nil, &Client{T: cannedDoor{}}); rs.State != "waiting" || rs.RevChecked {
		t.Errorf("threads unreadable: %+v", rs)
	}
	stubRev(t, "r9", errors.New("keel down")) // a value with the error must not be used
	if rs, _ := roundState(t, roundKeys(true, true, nil), closedDoc, nil); rs.State != "waiting" || rs.RevChecked {
		t.Errorf("rev unreadable: %+v", rs)
	}
}

// N3: a hand-written review_state does not survive a round; an equal one is
// not rewritten.
func TestReviewRoundRewritesAHandWrittenState(t *testing.T) {
	stubRev(t, "r9", nil)
	forged := json.RawMessage(`{"state":"pass","doc":"d.md","rev":"r9","rev_checked":true,"open":0}`)
	rs, set := roundState(t, roundKeys(true, false, forged), closedDoc, nil)
	if !set || rs.State != "waiting" {
		t.Fatalf("forged pass kept: set %v %+v", set, rs)
	}
	computed, _ := roundState(t, roundKeys(true, false, nil), closedDoc, nil)
	if _, set := roundState(t, roundKeys(true, false, model.MustJSON(computed)), closedDoc, nil); set {
		t.Fatal("an equal state was rewritten")
	}
	rep := &plugin.Reply{}
	w := &model.Critic{Name: "k", Mode: model.ModeKeel, Keel: &model.KeelRef{Repo: repo}}
	reviewRound(context.Background(), nil, rep, w, &model.KeelState{}, map[string]json.RawMessage{ReviewStateKey: forged}, nil)
	if len(rep.Unset) != 1 || rep.Unset[0] != ReviewStateKey {
		t.Fatalf("no review configured: unset %v", rep.Unset)
	}
}
