package keel

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

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
		if got := reviewState(cfg, c.verdicts, c.index, c.rev); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	// No reviewers named: every role with a verdict is required; none at all waits.
	if got := reviewState(ReviewConfig{Doc: "d.md"}, map[string]Verdict{"reviewer": pass}, closed, "r2"); got.State != "pass" {
		t.Errorf("implicit pool: %+v", got)
	}
	if got := reviewState(ReviewConfig{Doc: "d.md"}, map[string]Verdict{"reviewer": pass}, closed, ""); got.State != "waiting" || got.RevChecked {
		t.Errorf("pass verdict, unknown rev: %+v, want waiting", got)
	}
	if got := reviewState(ReviewConfig{Doc: "d.md"}, nil, closed, "r2"); got.State != "waiting" {
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

func TestReviewRoundWritesOnlyOnChangeAndKeepsTheOpenCount(t *testing.T) {
	old := DocRev
	DocRev = func(context.Context, *Client, string, string) (string, error) { return "r9", nil }
	defer func() { DocRev = old }()
	w := &model.Critic{Name: "k", Mode: model.ModeKeel, Keel: &model.KeelRef{Repo: repo}}
	st := &model.KeelState{}
	keys := map[string]json.RawMessage{
		ReviewKey:  json.RawMessage(`{"doc":"d.md"}`),
		VerdictKey: json.RawMessage(`{"reviewer":{"verdict":"pass","rev":"r9"}}`),
	}
	idx := map[string]IndexEntry{"a": {Path: "d.md"}}
	rep := &plugin.Reply{}
	reviewRound(context.Background(), nil, rep, w, st, keys, idx)
	var rs ReviewState
	_ = json.Unmarshal(rep.Set[ReviewStateKey], &rs)
	if rs.State != "waiting" || rs.Open != 1 || st.ReviewOpen != 1 {
		t.Fatalf("first: %+v state %+v", rs, st)
	}
	// A round that fetched nothing, same verdicts: nothing rewritten.
	rep = &plugin.Reply{}
	reviewRound(context.Background(), nil, rep, w, st, keys, nil)
	if _, set := rep.Set[ReviewStateKey]; set {
		t.Fatalf("unchanged state rewritten")
	}
	// The thread resolves: pass.
	rep = &plugin.Reply{}
	reviewRound(context.Background(), nil, rep, w, st, keys, map[string]IndexEntry{"a": {Path: "d.md", Resolved: true}})
	_ = json.Unmarshal(rep.Set[ReviewStateKey], &rs)
	if rs.State != "pass" || rs.Open != 0 {
		t.Fatalf("resolved: %+v", rs)
	}
	// The review key goes away: review_state is removed.
	rep = &plugin.Reply{}
	reviewRound(context.Background(), nil, rep, w, st, map[string]json.RawMessage{}, nil)
	if len(rep.Unset) != 1 || rep.Unset[0] != ReviewStateKey || st.ReviewLast != "" {
		t.Fatalf("unset %v last %q", rep.Unset, st.ReviewLast)
	}
}
