package scout

import (
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
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

// The positive control the Gatekeeper asked for (11-13): RC then APPROVE is not blocked; APPROVE then RC is. The
// blocked state overrides the step order, names the RC's head, and pings the owner role once per RC comment.
func TestArchitectRCBlocks(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: builder}}
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
	if len(r.Pings) != 1 || r.Pings[0].To != builder || !strings.Contains(r.Pings[0].Text, "#101") || !strings.Contains(r.Pings[0].Text, "REQUEST_CHANGES at aaaaaaaa") {
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

// A hold (a pass in progress) is no state of its own (rule.merge_ready): with no approval at the head the PR waits on
// the bot approval (ar shows the hold); any bot approval at the head is ready; an RC at an older head no longer
// blocks (the fix is pushed); a conflict still comes first.
func TestArchitectHold(t *testing.T) {
	w := newWorld(t)
	p := w.gh.items[101]
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "APPROVE"), archHold("2026-11-12T11:00:00Z", headA)}
	e := entry(t, w.pass(0), "pr:101")
	if e["ms"] != "waiting: bot approval" || vs(e["ar"]) != "hold@<nil>@2026-11-12T11:00:00Z" {
		t.Fatalf("hold at head: ms %v ar %s", e["ms"], vs(e["ar"]))
	}
	p.reviews = []map[string]any{{"author": map[string]any{"login": "lukas"}, "state": "APPROVED", "commit": map[string]any{"oid": headA}}}
	p.updated = w.now.Add(time.Minute)
	if e = entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "ready" {
		t.Fatalf("hold at head, lukas approved: ms %v", e["ms"])
	}
	p.reviews = nil
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "REQUEST_CHANGES"), archHold("2026-11-12T11:00:00Z", headA)}
	p.updated = w.now.Add(time.Minute)
	if e = entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "waiting: bot approval" {
		t.Fatalf("RC at an older head, then hold at head: ms %v", e["ms"])
	}
	p.mergeable, p.mergeState = "CONFLICTING", "DIRTY"
	p.updated = w.now.Add(time.Minute)
	if e = entry(t, w.pass(2*time.Minute), "pr:101"); e["ms"] != "waiting: conflict" {
		t.Fatalf("conflict: ms %v", e["ms"])
	}
}

// Seeding (Gatekeeper 11-13): the first pass that knows RC pings records the RCs already standing as pinged, so a deploy
// pings nobody; only an RC arriving after that pass pings.
func TestArchitectRCSeed(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: builder}, {Author: his, Role: secretary}}
	w.gh.items[102].draft = false
	for _, n := range []int{101, 102, 201} {
		w.gh.items[n].comments = []map[string]any{archComment("2026-11-10T20:00:00Z", w.gh.items[n].head, "REQUEST_CHANGES")}
	}
	r := w.pass(0)
	if len(r.Pings) != 0 {
		t.Fatalf("standing RCs pinged on the first pass: %v", r.Pings)
	}
	for k, h := range map[string]string{"pr:101": "aaaaaaaa", "pr:102": "bbbbbbbb", "pr:201": "cccccccc"} {
		if e := entry(t, r, k); e["ms"] != "blocked: architect RC at "+h {
			t.Fatalf("%s ms %v", k, e["ms"])
		}
	}
	if w.st.RCSince == "" {
		t.Fatal("no seed time recorded")
	}
	p := w.gh.items[101]
	p.comments = append(p.comments, archComment(w.now.Add(5*time.Minute).Format(time.RFC3339), headA, "REQUEST_CHANGES"))
	p.updated = w.now.Add(5 * time.Minute)
	r = w.pass(10 * time.Minute)
	if len(r.Pings) != 1 || r.Pings[0].To != builder || !strings.Contains(r.Pings[0].Text, "#101") {
		t.Fatalf("a new RC after the seed: pings %v", r.Pings)
	}
}

// An architect RC on a PR whose base is not main is recorded on the form (quiet_pings), never pinged; the same RC on
// main pings (Builder wake noise, 2026-11-17).
func TestArchitectRCOffMainIsQuiet(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: builder}}
	p := w.gh.items[101]
	p.base = "stack/x"
	p.comments = []map[string]any{archComment("2026-11-12T10:00:00Z", headB, "APPROVE")}
	w.pass(0)
	p.comments = append(p.comments, archComment("2026-11-12T11:00:00Z", headA, "REQUEST_CHANGES"))
	p.updated = w.now.Add(time.Minute)
	r := w.pass(2 * time.Minute)
	if e := entry(t, r, "pr:101"); e["ms"] != "blocked: architect RC at aaaaaaaa" {
		t.Fatalf("ms %v (log %v)", e["ms"], r.Log)
	}
	if q := string(r.Set["quiet_pings"]); len(r.Pings) != 0 || !strings.Contains(q, "#101") || !strings.Contains(q, "BLOCKED") || !strings.Contains(q, builder) {
		t.Fatalf("off main: pings %v, quiet %s", r.Pings, q)
	}
}
