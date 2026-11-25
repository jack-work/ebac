package ghcache

import (
	"slices"
	"strings"
	"testing"
)

// #71s finding 3 (strategist ruling: blocking): an ended entity's form is a read-only mirror, and a seat's edit of a
// write field on it is reverted; each field so dropped raises ONE alarm, sync.alarm.ended-edit.<field>, on the form.
// Synthesized fixtures only (acme/widget, as in sync_test.go).

const endedComment = "IC_kwSYNTH000700000001"

// endedWorld: #7 (ours) with one conversation comment by reviewer, agreed, then merged, read and shed. It returns
// the world, #7's key and its PR.
func endedWorld(t *testing.T) (*writeWorld, string, *PR) {
	w := newWriteWorld(t, explicitPRs)
	pr := w.gh.prs["acme/widget"][7]
	pr.Comments.Nodes = []Comment{{ID: endedComment, Author: &Login{Login: "reviewer"}, Body: "looks fine",
		CreatedAt: "2026-11-19T05:00:00Z", UpdatedAt: "2026-11-19T05:00:00Z", URL: "https://example.invalid/c/1"}}
	pr.Comments.TotalCount = 1
	bump(pr)
	w.round() // read with its comment
	w.round() // lands
	pr.State, pr.Mergeable, pr.MergeSt = "MERGED", "UNKNOWN", "UNKNOWN"
	bump(pr)
	w.round() // read merged: the end
	w.round() // lands; shed
	k := keyOf(KindPR, "acme/widget", 7)
	if e := w.st.Entities[k]; e == nil || e.EndedAt.IsZero() || e.Base != nil {
		t.Fatalf("#7 should be ended and shed: %+v", e)
	}
	return w, k, pr
}

// deepAgain moves #7's mark past ColdEvery, so the next round reads it deep and writes its form from GitHub.
func deepAgain(w *writeWorld, pr *PR) {
	w.now = w.now.Add(ColdEvery)
	bump(pr)
	w.round()
}

func endedAlarms(w *writeWorld, k string) []string {
	var out []string
	for key := range w.st.Entities[k].Alarms {
		if strings.HasPrefix(key, "ended-edit.") {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

func TestAnEditOnAnEndedFormIsRevertedWithOneAlarmPerField(t *testing.T) {
	w, k, pr := endedWorld(t)
	w.mutations()
	w.edit(k, func(g map[string]any) {
		addLabel("blocked")(g)
		g["draft"] = true
		th := thread(g, "PRRT_7_1")
		th["resolved"] = true
		th["replies"] = []any{map[string]any{"body": "a late reply"}}
		c := g["comments"].(map[string]any)[endedComment].(map[string]any)
		c["body"] = "looks fine, edited"
	})
	deepAgain(w, pr)
	want := []string{"ended-edit.comment-" + endedComment, "ended-edit.draft", "ended-edit.labels",
		"ended-edit.thread-PRRT_7_1-replies", "ended-edit.thread-PRRT_7_1-resolved"}
	if got := endedAlarms(w, k); !slices.Equal(got, want) {
		t.Fatalf("ended-edit alarms = %v, want %v", got, want)
	}
	if n := len(w.formSync(k).Alarm["ended-edit"]); n != 5 {
		t.Fatalf("the form's sync.alarm.ended-edit has %d fields, want 5: %v", n, w.formSync(k).Alarm)
	}
	f := w.formGH(k)
	if !slices.Equal(f.Labels, []string{"area:core"}) || f.Draft || f.Threads["PRRT_7_1"].Resolved || len(f.Threads["PRRT_7_1"].Replies) != 0 ||
		f.Comments[endedComment].Body != "looks fine" {
		t.Fatalf("the edit was not reverted: labels %v draft %v thread %+v comment %q", f.Labels, f.Draft, f.Threads["PRRT_7_1"], f.Comments[endedComment].Body)
	}
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("an ended form's edit reached GitHub: %v", m)
	}
	if len(w.r.Pings) != 1 {
		t.Fatalf("pings = %d, want one wake for the round's alarms", len(w.r.Pings))
	}
	deepAgain(w, pr) // the form is GitHub's again: nothing new
	if got := endedAlarms(w, k); !slices.Equal(got, want) || len(w.r.Pings) != 0 {
		t.Fatalf("a quiet ended round: alarms %v, pings %d", got, len(w.r.Pings))
	}
}

// GitHub moving under an ended form is not a seat's edit: labels, draft, resolution and a comment edited upstream
// (its updated_at moves) raise no ended-edit alarm.
func TestGitHubMovingUnderAnEndedFormRaisesNoAlarm(t *testing.T) {
	w, k, pr := endedWorld(t)
	pr.Labels.Nodes = append(pr.Labels.Nodes, struct {
		Name string `json:"name"`
	}{Name: "needs-review"})
	pr.Labels.TotalCount++
	pr.Draft = true
	pr.Threads.Nodes[0].Resolved = true
	pr.Comments.Nodes[0].Body, pr.Comments.Nodes[0].UpdatedAt = "looks fine (edited by its author)", "2026-11-21T05:00:00Z"
	deepAgain(w, pr)
	if got := endedAlarms(w, k); len(got) != 0 {
		t.Fatalf("GitHub's own moves raised %v", got)
	}
	if f := w.formGH(k); !slices.Equal(f.Labels, []string{"area:core", "needs-review"}) || !f.Draft || !f.Threads["PRRT_7_1"].Resolved {
		t.Fatalf("the form does not show GitHub's moves: %+v", f)
	}
	deepAgain(w, pr) // and they are the new mirror: still nothing
	if got := endedAlarms(w, k); len(got) != 0 {
		t.Fatalf("the next round raised %v", got)
	}
}

// A write of an ended form that does not land leaves the form as it was: what it shows is unknown, so the next
// round judges no mirror field against the write that never landed (GitHub's upstream label is no seat's edit).
func TestAnUnlandedEndedWriteJudgesNoMirror(t *testing.T) {
	w, k, pr := endedWorld(t)
	pr.Labels.Nodes = append(pr.Labels.Nodes, struct {
		Name string `json:"name"`
	}{Name: "needs-review"})
	pr.Labels.TotalCount++
	w.moveAt, w.moveKey = k, "sync" // a seat writes sync between the core's read and its write: the write does not land
	deepAgain(w, pr)
	if f := w.formGH(k); slices.Contains(f.Labels, "needs-review") {
		t.Fatalf("the write landed: %v", f.Labels)
	}
	w.round() // redone
	if got := endedAlarms(w, k); len(got) != 0 {
		t.Fatalf("an unlanded write read as a seat's edit: %v", got)
	}
}

// An entity that ends without a deep read (it left its search, still open) takes its mirror from the form as read
// that round: a later edit of its labels is reverted with the alarm.
func TestAnEntityEndedByItsSearchMirrorsItsForm(t *testing.T) {
	const q = "repo:acme/widget is:pr is:open author:our-author"
	w := newWriteWorld(t, `{"repos":["acme/widget"],"rules":[{"author":["our-author"]}]}`)
	w.gh.search[q] = []int{7}
	w.round()
	w.round()
	k := keyOf(KindPR, "acme/widget", 7)
	if e := w.st.Entities[k]; e == nil || e.Base == nil {
		t.Fatalf("#7 not tracked with a base: %+v", e)
	}
	w.gh.search[q] = nil // it leaves the search, open: untracked
	w.round()
	if e := w.st.Entities[k]; e == nil || e.EndedAt.IsZero() || e.Ended == nil || len(w.r.Telemetry.Deep) != 0 {
		t.Fatalf("#7 should end unread with a mirror: %+v, deep %v", e, w.r.Telemetry.Deep)
	}
	w.edit(k, addLabel("blocked"))
	deepAgain(w, w.gh.prs["acme/widget"][7])
	if got := endedAlarms(w, k); !slices.Equal(got, []string{"ended-edit.labels"}) {
		t.Fatalf("ended-edit alarms = %v, want [ended-edit.labels]", got)
	}
}
