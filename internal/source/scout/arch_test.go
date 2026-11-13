package scout

import (
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/mergecheck"
	"github.com/gluck/ebac/internal/model"
)

// archComment is an architect-review[bot] verdict comment on #101, shaped as the bot posts it (#110281, 11-12).
func archComment(at, sha, verdict string) map[string]any {
	return map[string]any{"author": map[string]any{"login": "architect-review[bot]"}, "createdAt": at,
		"body": "<!-- architect-review -->\n## Architect review \u2014 PR #101 @ `" + sha + "`\n\n**" + verdict + "**\n\nfindings"}
}

func archHold(at, sha string) map[string]any {
	return map[string]any{"author": map[string]any{"login": "architect-review[bot]"}, "createdAt": at,
		"body": "<!-- architect-review-hold -->\n\U0001F6AB **MERGE BLOCKED \u2014 Architect review in progress.**\nHead: " + sha + "\n"}
}

// The positive control the Warden asked for (11-13): RC then APPROVE is not blocked; APPROVE then RC is. The
// blocked state overrides the step order, names the RC's head, and pings the owner role once per RC comment.
func TestArchitectRCBlocks(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: knight}}
	p := w.gh.items[101]
	bump := func() { p.updated = w.now.Add(time.Minute) }

	// RC, then a later APPROVE comment: not blocked, no ping.
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "REQUEST_CHANGES"), archComment("2026-11-12T11:00:00Z", headA, "APPROVE")}
	r := w.pass(0)
	if e := entry(t, r, "pr:101"); strings.HasPrefix(e["ms"].(string), "blocked") || len(r.Pings) != 0 {
		t.Fatalf("RC then APPROVE: ms %v, pings %v", e["ms"], r.Pings)
	}

	// APPROVE, then a later RC: blocked at the RC's head, owner pinged once.
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "APPROVE"), archComment("2026-11-12T11:00:00Z", headA, "REQUEST_CHANGES")}
	bump()
	r = w.pass(2 * time.Minute)
	if e := entry(t, r, "pr:101"); e["ms"] != "blocked: architect RC at aaaaaaaa" {
		t.Fatalf("APPROVE then RC: ms %v (log %v)", e["ms"], r.Log)
	}
	if len(r.Pings) != 1 || r.Pings[0].To != knight || !strings.Contains(r.Pings[0].Text, "#101") || !strings.Contains(r.Pings[0].Text, "REQUEST_CHANGES at aaaaaaaa") {
		t.Fatalf("pings %v", r.Pings)
	}
	bump() // re-read (a comment elsewhere): the same RC pings no more
	if r = w.pass(2 * time.Minute); len(r.Pings) != 0 {
		t.Fatalf("the same RC pinged again: %v", r.Pings)
	}

	// A second RC comment (a fresh pass, still REQUEST_CHANGES) pings again.
	p.comments = append(p.comments, archComment("2026-11-12T13:00:00Z", headA, "REQUEST_CHANGES"))
	bump()
	if r = w.pass(2 * time.Minute); len(r.Pings) != 1 {
		t.Fatalf("a new RC comment: pings %v", r.Pings)
	}

	// The bot's APPROVE landing as an Ilya-Grebnov review after the RC supersedes it; one before it does not.
	p.reviews = []map[string]any{{"author": map[string]any{"login": "Ilya-Grebnov"}, "state": "APPROVED", "submittedAt": "2026-11-12T12:00:00Z", "commit": map[string]any{"oid": headA}}}
	bump()
	if e := entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "blocked: architect RC at aaaaaaaa" {
		t.Fatalf("an Ilya approve BEFORE the RC unblocked: ms %v", e["ms"])
	}
	p.reviews[0]["submittedAt"] = "2026-11-12T14:00:00Z"
	bump()
	if e := entry(t, w.pass(2*time.Minute), "pr:101"); strings.HasPrefix(e["ms"].(string), "blocked") {
		t.Fatalf("an Ilya approve AFTER the RC still blocked: ms %v", e["ms"])
	}
}

// A hold (a pass in progress) on the PR's own head reads "waiting: architect review"; a hold naming an older head
// leaves the state to the steps; a conflict still comes first; an unsuperseded RC wins over a hold.
func TestArchitectHold(t *testing.T) {
	w := newWorld(t)
	p := w.gh.items[101]
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "APPROVE"), archHold("2026-11-12T11:00:00Z", headA)}
	e := entry(t, w.pass(0), "pr:101")
	if e["ms"] != "waiting: architect review" || vs(e["ar"]) != "hold@<nil>@2026-11-12T11:00:00Z" {
		t.Fatalf("hold at head: ms %v ar %s", e["ms"], vs(e["ar"]))
	}
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "APPROVE"), archHold("2026-11-12T11:00:00Z", headB)}
	p.updated = w.now.Add(time.Minute)
	e = entry(t, w.pass(2*time.Minute), "pr:101")
	if e["ms"] == "waiting: architect review" || vs(e["ar"]) != "hold@bbbbbbbb@2026-11-12T11:00:00Z" {
		t.Fatalf("stale hold: ms %v ar %s", e["ms"], vs(e["ar"]))
	}
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "REQUEST_CHANGES"), archHold("2026-11-12T11:00:00Z", headA)}
	p.updated = w.now.Add(time.Minute)
	if e = entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "blocked: architect RC at bbbbbbbb" {
		t.Fatalf("RC then hold at head: ms %v", e["ms"])
	}
	p.mergeable, p.mergeState = "CONFLICTING", "DIRTY"
	p.updated = w.now.Add(time.Minute)
	if e = entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "waiting: conflict" {
		t.Fatalf("conflict and RC: ms %v", e["ms"])
	}
}

// ArchState is open-only and needs a verdict; an RC with an unparseable time is never superseded (fail closed).
func TestArchState(t *testing.T) {
	open := &mergecheck.Facts{State: "OPEN", Oid: headA}
	rc := &mergecheck.Arch{Verdict: "REQUEST_CHANGES", Sha: headB, At: "2026-11-12T10:00:00Z"}
	if s, b := mergecheck.ArchState(open, rc); s != "blocked: architect RC at bbbbbbbb" || !b {
		t.Fatalf("rc: %q %v", s, b)
	}
	for _, st := range []string{"MERGED", "CLOSED"} {
		if s, b := mergecheck.ArchState(&mergecheck.Facts{State: st}, rc); s != "" || b {
			t.Fatalf("%s: %q %v", st, s, b)
		}
	}
	if s, _ := mergecheck.ArchState(open, nil); s != "" {
		t.Fatalf("nil arch: %q", s)
	}
	for _, v := range []string{"APPROVE", "APPROVE_WITH_NOTES", "COMMENT", "UNREAD"} {
		if s, b := mergecheck.ArchState(open, &mergecheck.Arch{Verdict: v, Sha: headB, At: rc.At}); s != "" || b {
			t.Fatalf("%s: %q %v", v, s, b)
		}
	}
	other := &mergecheck.Facts{State: "OPEN", Oid: headA, Reviews: []mergecheck.Review{{Login: "someone", State: "APPROVED", At: "2026-11-12T12:00:00Z"}}}
	if _, b := mergecheck.ArchState(other, rc); !b {
		t.Fatal("a non-Ilya approve superseded the RC")
	}
	cr := &mergecheck.Facts{State: "OPEN", Oid: headA, Reviews: []mergecheck.Review{{Login: "Ilya-Grebnov", State: "CHANGES_REQUESTED", At: "2026-11-12T12:00:00Z"}}}
	if _, b := mergecheck.ArchState(cr, rc); !b {
		t.Fatal("an Ilya CHANGES_REQUESTED superseded the RC")
	}
	bad := &mergecheck.Arch{Verdict: "REQUEST_CHANGES", Sha: headB, At: "garbage"}
	ilya := &mergecheck.Facts{State: "OPEN", Oid: headA, Reviews: []mergecheck.Review{{Login: "Ilya-Grebnov", State: "APPROVED", At: "2026-11-12T12:00:00Z"}}}
	if _, b := mergecheck.ArchState(ilya, bad); !b {
		t.Fatal("an RC with an unparseable time was superseded")
	}
}
