package form_test

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/source/github"
	"github.com/gluck/ebac/sdk/model"
)

const formID = "@ac44d671aaaaaaaaaaaaaaaaaaaaaaaa"

func cmt(id, by, body string) *model.Comment {
	c := &model.Comment{ID: id, Author: by, Body: body, URL: "https://x/c/" + id}
	c.ComputeDigest()
	return c
}

// round1 is a busy PR: two threads (one open), a review, a conversation
// comment, a bot.
func round1() *model.Snapshot {
	pr := &model.PRState{Key: "acme/widget#1", Owner: "acme", Repo: "widget", Number: 1, URL: "https://x/pull/1",
		Title: "t", Author: "jack", State: "OPEN", HeadSHA: "aaaaaaaaaaaa", Complete: true,
		Threads: map[string]*model.Thread{
			"T1": {ID: "T1", Comments: []*model.Comment{cmt("C1", "amy", "finding one"), cmt("C2", "jack", "fixed")}},
			"T2": {ID: "T2", Comments: []*model.Comment{cmt("C3", "bob", "finding two")}},
		},
		Reviews:  map[string]*model.Review{"R1": {ID: "R1", Author: "amy", State: "COMMENTED", Body: "see threads"}},
		Comments: map[string]*model.Comment{"IC1": cmt("IC1", "bot", "ci green")},
	}
	pr.Comments["IC1"].IsBot = true
	return &model.Snapshot{Critic: "c", Round: 1, TakenAt: time.Unix(100, 0).UTC(), Complete: true,
		PRs: map[model.PRKey]*model.PRState{pr.Key: pr}}
}

// round2 changes every kind of thing: a reply, an edit, a resolve, a new
// review, a new conversation comment.
func round2() *model.Snapshot {
	s := round1()
	s.Round = 2
	pr := s.PRs["acme/widget#1"]
	pr.Threads["T2"].Comments = append(pr.Threads["T2"].Comments, cmt("C4", "carl", "agree with bob"))
	pr.Threads["T1"].Comments[0] = cmt("C1", "amy", "finding one, edited")
	pr.Threads["T1"].IsResolved = true
	pr.Reviews["R2"] = &model.Review{ID: "R2", Author: "bob", State: "CHANGES_REQUESTED", Body: "blocking"}
	pr.Comments["IC2"] = cmt("IC2", "dana", "ping")
	return s
}

func kinds(d *model.Delta) []string {
	var out []string
	for _, e := range d.Events {
		out = append(out, string(e.Kind))
	}
	sort.Strings(out)
	return out
}

var wantRound2 = []string{"comment_edited", "comment_new", "pr_comment_new", "review_submitted", "thread_resolved"}

// A form written by the old binary (the whole tree on prs) migrates on its
// first round: the round diffs against the form's tree, writes heads and the
// detail file, and the next round finds every new comment, edit, resolve and
// review against the file exactly as it did against the old form.
func TestPRDetailMovesToDiskAndRoundsStillSeeEverything(t *testing.T) {
	st := &form.Store{Root: t.TempDir()}
	legacy := map[string]json.RawMessage{"prs": form.LegacyPRsForTest(round1()),
		"system": json.RawMessage(`{"ebac":{"snapshot_round":1,"snapshot_taken_at":"1970-01-01T00:01:40Z"}}`)}

	// The baseline: round 2 against the old form, as ebac diffed before.
	oldPrev, err := form.ReadFormForTest(st, formID, legacy).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(github.Diff(oldPrev, round2(), "jack")); strings.Join(got, ",") != strings.Join(wantRound2, ",") {
		t.Fatalf("baseline against the old form: %v, want %v", got, wantRound2)
	}

	// Migration round: nothing changed upstream, so nothing is reported; the
	// save writes heads and the file.
	if d := github.Diff(oldPrev, round1(), "jack"); len(d.Events) != 0 {
		t.Fatalf("an unchanged migration round reported %v", kinds(d))
	}
	keys := map[string]json.RawMessage{"prs": legacy["prs"]}
	if err := form.SaveRoundPRsForTest(st, formID, keys, round1()); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{`"threads":{`, `"reviews":{`, `"comments":{`, `"C1"`, `"T1"`, `"d":`} {
		if strings.Contains(string(keys["prs"]), leak) {
			t.Fatalf("detail %s still on the form: %s", leak, keys["prs"])
		}
	}
	if len(keys["prs"]) >= len(legacy["prs"]) {
		t.Fatalf("heads %d B are not smaller than the tree %d B", len(keys["prs"]), len(legacy["prs"]))
	}
	if _, err := os.Stat(form.PRsPathForTest(st, formID)); err != nil {
		t.Fatalf("no detail file: %v", err)
	}

	// The next round diffs against the file and sees everything.
	prev, err := form.ReadFormForTest(st, formID, keys).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(github.Diff(prev, round2(), "jack")); strings.Join(got, ",") != strings.Join(wantRound2, ",") {
		t.Fatalf("after the move: %v, want %v", got, wantRound2)
	}
	// ...and an unchanged round is still quiet.
	if d := github.Diff(prev, round1(), "jack"); len(d.Events) != 0 {
		t.Fatalf("an unchanged round after the move reported %v", kinds(d))
	}
	// The outbox check still finds the threads a reply names.
	if prev.PRs["acme/widget#1"].Threads["T2"] == nil {
		t.Fatal("thread T2 is not in the snapshot read back")
	}

	// A form rolled back to a tree (an older ebac wrote it after the move)
	// is newer than the file: the form wins.
	newer := round2()
	rolled := map[string]json.RawMessage{"prs": form.LegacyPRsForTest(newer)}
	got, err := form.ReadFormForTest(st, formID, rolled).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got.PRs["acme/widget#1"].Reviews["R2"] == nil {
		t.Fatal("the file beat a newer tree on the form")
	}
}

// The detail file lost (deleted, or its write failed on the migration
// round): the heads alone must not read as a PR whose every thread is new.
// The control is the same round against heads with no NoDetail mark (what a
// reader that ignored the mark would do): a flood.
func TestLostDetailRebaselinesWithoutFlood(t *testing.T) {
	st := &form.Store{Root: t.TempDir(), Warn: &bytes.Buffer{}}
	keys := map[string]json.RawMessage{}
	if err := form.SaveRoundPRsForTest(st, formID, keys, round1()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(form.PRsPathForTest(st, formID)); err != nil {
		t.Fatal(err)
	}
	prev, err := form.ReadFormForTest(st, formID, keys).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	p := prev.PRs["acme/widget#1"]
	if p == nil || !p.NoDetail || len(p.Threads) != 0 {
		t.Fatalf("heads read back as %+v", p)
	}
	d := github.Diff(prev, round2(), "jack")
	if len(d.Events) != 1 || d.Events[0].Kind != model.EvThreadNew || d.Events[0].Tier != model.TierRecord || d.Events[0].ThreadID != "" {
		t.Fatalf("lost detail: %v, want one record re-baseline", kinds(d))
	}
	p.NoDetail = false
	if flood := github.Diff(prev, round2(), "jack"); len(flood.Events) < 5 || !flood.WakeWorthy() {
		t.Fatalf("control: heads without the mark did not flood: %v", kinds(flood))
	}

	// The mark survives a round that carries the PR forward unfetched (a
	// failed fetch reuses the previous state): written to the file, read
	// back, still re-baselined.
	p.NoDetail = true
	carried := &model.Snapshot{Round: 2, TakenAt: time.Unix(200, 0).UTC(), PRs: prev.PRs}
	if err := form.SaveRoundPRsForTest(st, formID, keys, carried); err != nil {
		t.Fatal(err)
	}
	again, err := form.ReadFormForTest(st, formID, keys).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !again.PRs["acme/widget#1"].NoDetail {
		t.Fatal("NoDetail lost across a carried-forward round")
	}
}
