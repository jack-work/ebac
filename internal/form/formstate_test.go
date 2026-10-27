package form

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	"github.com/jack-work/figaro/api/form"
)

// 2026-10-20: a form whose system member was a leaf lost its whole system
// subtree to a nested write (jack-work/figaro#43). Writes must not nest.
func TestNextBoardSurvivesALeafSystem(t *testing.T) {
	var cur form.Snapshot
	if err := json.Unmarshal([]byte(`{"name":"f","system":null,"post":{"p1":{"pr":"a/b#1","body":"x"}}}`), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := nextBoard(cur, map[string]json.RawMessage{"system.ebac": json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("a dotted key was accepted")
	}
	curKeys := map[string]json.RawMessage{}
	b, _ := json.Marshal(cur)
	_ = json.Unmarshal(b, &curKeys)

	system := model.ObjectOf(curKeys["system"])
	system["ebac"] = json.RawMessage(`{"runtime":{"round":41}}`)
	posts := model.ObjectOf(curKeys["post"])
	p1 := model.ObjectOf(posts["p1"])
	p1["sent"] = json.RawMessage(`{"url":"u"}`)
	posts["p1"] = model.MustJSON(p1)
	next, err := nextBoard(cur, map[string]json.RawMessage{"system": model.MustJSON(system), "post": model.MustJSON(posts)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(next)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(out, &got)
	for k := range got {
		if strings.Contains(k, ".") {
			t.Errorf("flat decoy key %q in %s", k, out)
		}
	}
	if string(got["system"]) != `{"ebac":{"runtime":{"round":41}}}` {
		t.Errorf("system = %s", got["system"])
	}
	if string(got["post"]) != `{"p1":{"body":"x","pr":"a/b#1","sent":{"url":"u"}}}` {
		t.Errorf("post = %s", got["post"])
	}
}

func TestPrsTreeKeepsOrderAndDigestsButNoBodies(t *testing.T) {
	_, snap := outboxCritic()
	pr := snap.PRs["acme/widget#1"]
	c1 := &model.Comment{ID: "C1", Author: "a", Body: "first", CreatedAt: "2026-10-20T01:00:00Z"}
	c2 := &model.Comment{ID: "C2", Author: "b", Body: "second", CreatedAt: "2026-10-20T02:00:00Z", State: "PENDING"}
	c1.ComputeDigest()
	c2.ComputeDigest()
	pr.Threads["PRRT_1"].Comments = []*model.Comment{c1, c2}
	pr.Threads["PRRT_1"].IsResolved = true
	pr.Comments["IC1"] = &model.Comment{ID: "IC1", Author: "c", Body: "top"}
	pr.Reviews["R1"] = &model.Review{ID: "R1", Author: "d", State: "APPROVED", Body: "lgtm"}
	snap.PRs["acme/web.site#2"] = &model.PRState{Key: "acme/web.site#2", Owner: "acme", Repo: "web.site", Number: 2}

	raw := model.MustJSON(prsTree(snap))
	for _, body := range []string{"first", "second", "top", "lgtm"} {
		if strings.Contains(string(raw), `"`+body+`"`) {
			t.Fatalf("body %q reached the form: %s", body, raw)
		}
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	for k := range keys {
		if strings.Contains(k, ".") {
			t.Fatalf("a dot in a repo name reached the form path: %q", k)
		}
	}
	got, err := prsFromTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["acme/web.site#2"]; !ok {
		t.Fatalf("dotted repo lost: keys %v", got)
	}
	th := got["acme/widget#1"].Threads["PRRT_1"]
	if len(th.Comments) != 2 || th.Comments[0].ID != "C1" || th.Comments[1].ID != "C2" || !th.IsResolved {
		t.Fatalf("thread: %+v", th)
	}
	if th.Comments[0].Digest != c1.Digest || th.Comments[1].State != "PENDING" || th.Comments[0].Author != "a" {
		t.Fatalf("comment identity lost: %+v %+v", th.Comments[0], th.Comments[1])
	}
	if r := got["acme/widget#1"].Reviews["R1"]; r == nil || r.BodyDigest() != model.BodyDigest("lgtm") || r.State != "APPROVED" {
		t.Fatalf("review: %+v", r)
	}

	// The full objects ebac wrote before compaction still read.
	old := json.RawMessage(`{"acme/widget#1":{"key":"acme/widget#1","state":"OPEN","threads":{"T":{"is_resolved":true,"comments":{
	  "B":{"id":"B","author":"y","body":"two","created_at":"2","digest":"bbbb"},
	  "A":{"id":"A","author":"x","body":"one","created_at":"1","digest":"aaaa","state":"PENDING"}}}},
	  "reviews":{"R":{"id":"R","author":"z","state":"COMMENTED","body":"hm"}},"comments":{"I":{"id":"I","author":"w","body":"x","digest":"cccc"}}}}`)
	o, err := prsFromTree(old)
	if err != nil {
		t.Fatal(err)
	}
	ot := o["acme/widget#1"].Threads["T"]
	if len(ot.Comments) != 2 || ot.Comments[0].ID != "A" || ot.Comments[0].State != "PENDING" || !ot.IsResolved || ot.Comments[1].Digest != "bbbb" {
		t.Fatalf("old thread: %+v", ot)
	}
	if r := o["acme/widget#1"].Reviews["R"]; r.BodyDigest() != model.BodyDigest("hm") {
		t.Fatalf("old review digest %q", r.BodyDigest())
	}
}

// 2026-10-20: a deleted review and seven deleted comments reached the form as
// null leaves, and reading them back panicked every unit that loaded the PR.
func TestPrsFromTreeTreatsNullAsAbsent(t *testing.T) {
	raw := json.RawMessage(`{
	  "acme/widget#1": {"key":"acme/widget#1","owner":"acme","repo":"widget","number":1,
	    "threads": {
	      "T1": {"id":"T1","comments":{"C1":{"id":"C1","body":"kept","created_at":"1"},"C2":null,"C3":null}},
	      "T2": null
	    },
	    "reviews": {"R1":{"id":"R1","state":"APPROVED"},"R2":null},
	    "comments": {"I1":null}
	  },
	  "acme/gone#2": null
	}`)
	prs, err := prsFromTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %v", prs)
	}
	p := prs["acme/widget#1"]
	if len(p.Threads) != 1 || len(p.Threads["T1"].Comments) != 1 || p.Threads["T1"].Comments[0].ID != "C1" {
		t.Errorf("threads = %+v", p.Threads)
	}
	if len(p.Reviews) != 1 || p.Reviews["R1"] == nil || len(p.Comments) != 0 {
		t.Errorf("reviews = %v, comments = %v", p.Reviews, p.Comments)
	}
}

func outboxCritic() (*model.Critic, *model.Snapshot) {
	pr := &model.PRState{
		Key: model.MakePRKey("acme", "widget", 1), Owner: "acme", Repo: "widget", Number: 1, URL: "https://x/pull/1",
		State: "OPEN", Author: "friend", HeadSHA: "abc123",
		Threads:  map[string]*model.Thread{"PRRT_1": {ID: "PRRT_1", Path: "a.go", Line: 3}},
		Reviews:  map[string]*model.Review{},
		Comments: map[string]*model.Comment{},
	}
	w := &model.Critic{Name: "rw", Mode: model.ModeReview, Write: true, FormID: "@f"}
	return w, &model.Snapshot{PRs: map[model.PRKey]*model.PRState{pr.Key: pr}}
}
