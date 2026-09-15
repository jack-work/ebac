package main

import (
	"strings"
	"testing"
	"time"
)

func cmt(id, author, body string, bot bool) *Comment {
	c := &Comment{ID: id, Author: author, Body: body, IsBot: bot}
	c.computeDigest()
	return c
}

func snapWith(prs ...*PRState) *Snapshot {
	s := &Snapshot{PRs: map[PRKey]*PRState{}, Complete: true, TakenAt: time.Now()}
	for _, p := range prs {
		if p.Threads == nil {
			p.Threads = map[string]*Thread{}
		}
		if p.Reviews == nil {
			p.Reviews = map[string]*Review{}
		}
		p.Complete = true
		s.PRs[p.Key] = p
	}
	return s
}

func basePR() *PRState {
	return &PRState{
		Key: "acme/widget#1", Owner: "acme", Repo: "widget", Number: 1,
		State: "OPEN", HeadSHA: "aaaaaaaaaaaa", URL: "https://x/pull/1",
		Threads: map[string]*Thread{}, Reviews: map[string]*Review{}, Comments: map[string]*Comment{}, Complete: true,
	}
}

func kinds(d *Delta) []string {
	var out []string
	for _, e := range d.Events {
		out = append(out, string(e.Kind))
	}
	return out
}

func tierOf(d *Delta, kind EventKind) Tier {
	for _, e := range d.Events {
		if e.Kind == kind {
			return e.Tier
		}
	}
	return -1
}

func TestFirstRoundAdoptsWithoutAvalanche(t *testing.T) {
	pr := basePR()
	for i := 0; i < 30; i++ {
		id := string(rune('a' + i))
		pr.Threads[id] = &Thread{ID: id, Comments: []*Comment{cmt(id+"c", "someone", "hi", false)}}
	}
	d := Diff(nil, snapWith(pr), "me")

	// Exactly one wake event, not thirty: adopting a busy PR must not
	// detonate. The standing feedback is summarised as one record event.
	if got := len(d.Tier1()); got != 1 {
		t.Fatalf("adoption produced %d wake events, want 1: %v", got, kinds(d))
	}
	if d.Tier1()[0].Kind != EvPRAdded {
		t.Fatalf("wake event is %s, want pr_added", d.Tier1()[0].Kind)
	}
	if len(d.Events) != 2 {
		t.Fatalf("want 2 events (added + standing summary), got %d: %v", len(d.Events), kinds(d))
	}
}

func TestNewHumanCommentWakes(t *testing.T) {
	prev := basePR()
	prev.Threads["t1"] = &Thread{ID: "t1", Path: "a.go", Comments: []*Comment{cmt("c1", "alice", "first", false)}}
	next := basePR()
	next.Threads["t1"] = &Thread{ID: "t1", Path: "a.go", Comments: []*Comment{
		cmt("c1", "alice", "first", false),
		cmt("c2", "alice", "second", false),
	}}
	d := Diff(snapWith(prev), snapWith(next), "me")
	if !d.WakeWorthy() {
		t.Fatalf("a new human comment must wake; got %v", kinds(d))
	}
	if tierOf(d, EvCommentNew) != TierWake {
		t.Fatalf("comment_new tier = %d, want %d", tierOf(d, EvCommentNew), TierWake)
	}
}

// A reviewer's follow-up posted as a top-level PR conversation comment
// (GraphQL pullRequest.comments) rather than an inline review-thread reply
// is a real, observed shape (a real PR can carry several).
// Before FetchPR/diffPR knew about this connection at all, such a comment
// was invisible end to end: not fetched, not stored, not diffed, no event —
// a false absence a quiet monitor is built to have.
func TestNewTopLevelPRCommentWakes(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.Comments["pc1"] = cmt("pc1", "babrekel", "did you look at this yet?", false)

	d := Diff(snapWith(prev), snapWith(next), "me")
	if !d.WakeWorthy() {
		t.Fatalf("a new top-level PR comment must wake; got %v", kinds(d))
	}
	if tierOf(d, EvPRCommentNew) != TierWake {
		t.Fatalf("pr_comment_new tier = %d, want %d", tierOf(d, EvPRCommentNew), TierWake)
	}
}

// Our own top-level comments (the operator narrating their own PR, the
// common case in practice) must not wake the seat, same as thread comments.
func TestOwnTopLevelPRCommentDoesNotWake(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.Comments["pc1"] = cmt("pc1", "me", "commit 2 of 4 is in", false)

	d := Diff(snapWith(prev), snapWith(next), "me")
	if d.WakeWorthy() {
		t.Fatalf("own top-level comment woke the seat: %v", kinds(d))
	}
	if len(d.Events) == 0 {
		t.Fatal("own top-level comment was discarded entirely, not demoted")
	}
}

// Observed live in production use: the Automaton
// review bot posts a pure queue-position ping ("expect it typically about
// an hour and a half...") through a real human GitHub account ("bob" /
// Lukas Stankiewicz, __typename User, not Bot) -- isBot() cannot catch this
// by author identity, only the embedded "automaton-ack" marker can. It woke
// the seat for zero actionable content, twice in one round.
func TestAutomatonAckDoesNotWakeButRealVerdictDoes(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.Comments["ack1"] = cmt("ack1", "bob",
		"Automaton has queued this PR for review — expect it typically **about an hour and a half** after entering the queue, and usually within **a day**.\n"+
			"<!-- automaton-ack:23650:03adf36d -->", false)

	d := Diff(snapWith(prev), snapWith(next), "me")
	if d.WakeWorthy() {
		t.Fatalf("an automaton queue-ack woke the seat: %v", kinds(d))
	}
	if len(d.Events) == 0 {
		t.Fatal("automaton ack was discarded entirely, not demoted")
	}

	// Negative control: the SAME account's real verdict, carrying actual
	// findings, must still wake -- content is the signal, not the author.
	verdictPrev := basePR()
	verdictNext := basePR()
	verdictNext.Comments["v1"] = cmt("v1", "bob",
		"<!-- automaton:23654:0956a215 verdict:request_changes -->\n## Automaton Review: Request Changes", false)
	vd := Diff(snapWith(verdictPrev), snapWith(verdictNext), "me")
	if !vd.WakeWorthy() {
		t.Fatalf("a real automaton verdict did not wake: %v", kinds(vd))
	}
}

func TestBotAndSelfCommentsDoNotWake(t *testing.T) {
	for _, tc := range []struct {
		name, author string
		bot          bool
	}{
		{"bot", "coderabbitai", true},
		{"self", "me", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := basePR()
			prev.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "x", false)}}
			next := basePR()
			next.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{
				cmt("c1", "alice", "x", false),
				cmt("c2", tc.author, "y", tc.bot),
			}}
			d := Diff(snapWith(prev), snapWith(next), "me")
			if d.WakeWorthy() {
				t.Fatalf("%s comment woke the seat: %v", tc.name, kinds(d))
			}
			// It must still be RECORDED. Demotion is not discarding: a
			// silence and a background event are different answers.
			if len(d.Events) == 0 {
				t.Fatalf("%s comment was discarded entirely, not demoted", tc.name)
			}
		})
	}
}

func TestEditIsDetectedByDigestNotTimestamp(t *testing.T) {
	prev := basePR()
	prev.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{
		{ID: "c1", Author: "alice", Body: "before", UpdatedAt: "2026-01-01T00:00:00Z", Digest: digestOf("before")},
	}}
	next := basePR()
	// Same UpdatedAt on purpose: some edits do not move it. The digest must
	// still catch the change.
	next.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{
		{ID: "c1", Author: "alice", Body: "after", UpdatedAt: "2026-01-01T00:00:00Z", Digest: digestOf("after")},
	}}
	d := Diff(snapWith(prev), snapWith(next), "me")
	if tierOf(d, EvCommentEdited) != TierWake {
		t.Fatalf("edit not detected as a wake event: %v", kinds(d))
	}
}

func digestOf(s string) string {
	c := &Comment{Body: s}
	c.computeDigest()
	return c.Digest
}

// The single most important robustness property: a partial scan must never
// be allowed to look like a deletion. A timed-out request that reports a
// thread as removed is worse than no monitor at all.
func TestPartialScanNeverInfersDeletion(t *testing.T) {
	prev := basePR()
	prev.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "x", false)}}
	prev.Threads["t2"] = &Thread{ID: "t2", Comments: []*Comment{cmt("c2", "bob", "y", false)}}

	next := basePR()
	next.Complete = false // pagination did not finish
	next.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "x", false)}}

	ns := snapWith(next)
	ns.PRs[next.Key].Complete = false
	ns.Complete = false

	d := Diff(snapWith(prev), ns, "me")
	for _, e := range d.Events {
		if e.Kind == EvThreadGone {
			t.Fatalf("partial scan inferred a deletion: %v", e.Line())
		}
	}
	if len(d.SuppressedDeletions) == 0 {
		t.Fatal("suppression was silent; it must be reported as a distinct result")
	}
}

func TestCompleteScanDoesInferDeletion(t *testing.T) {
	// The negative control for the test above: without it, a differ that
	// never emits thread_deleted at all would pass.
	prev := basePR()
	prev.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "a", "x", false)}}
	prev.Threads["t2"] = &Thread{ID: "t2", Comments: []*Comment{cmt("c2", "b", "y", false)}}
	next := basePR()
	next.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "a", "x", false)}}

	d := Diff(snapWith(prev), snapWith(next), "me")
	found := false
	for _, e := range d.Events {
		if e.Kind == EvThreadGone {
			found = true
		}
	}
	if !found {
		t.Fatalf("complete scan failed to report a real deletion: %v", kinds(d))
	}
	if len(d.SuppressedDeletions) != 0 {
		t.Fatalf("complete scan suppressed deletions: %v", d.SuppressedDeletions)
	}
}

func TestHeadMoveWakesOnlyWithOutstandingFeedback(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.HeadSHA = "bbbbbbbbbbbb"
	d := Diff(snapWith(prev), snapWith(next), "me")
	if d.WakeWorthy() {
		t.Fatalf("push on a PR with no feedback woke the seat: %v", kinds(d))
	}

	prev2 := basePR()
	prev2.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "fix this", false)}}
	next2 := basePR()
	next2.HeadSHA = "bbbbbbbbbbbb"
	next2.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "fix this", false)}}
	d2 := Diff(snapWith(prev2), snapWith(next2), "me")
	if tierOf(d2, EvHeadMoved) != TierWake {
		t.Fatalf("push with unresolved feedback did not wake: %v", kinds(d2))
	}
}

func TestThreadReopenWakesResolveDoesNot(t *testing.T) {
	mk := func(resolved bool) *PRState {
		p := basePR()
		p.Threads["t1"] = &Thread{ID: "t1", IsResolved: resolved,
			Comments: []*Comment{cmt("c1", "alice", "x", false)}}
		return p
	}
	d := Diff(snapWith(mk(false)), snapWith(mk(true)), "me")
	if d.WakeWorthy() {
		t.Fatalf("someone resolving a thread woke the seat: %v", kinds(d))
	}
	d2 := Diff(snapWith(mk(true)), snapWith(mk(false)), "me")
	if !d2.WakeWorthy() {
		t.Fatalf("a re-opened thread must wake: %v", kinds(d2))
	}
}

// Observed live in production use: bob raised a finding and
// alice replied within the same poll gap, so diffPR saw this thread for
// the first time already holding both comments. Attributing the event to
// LastComment() credited alice (the reply) as having opened the thread
// and quoted his words as the finding, erasing bob's actual one entirely.
func TestThreadNewCreditsWhoeverOpenedItNotWhoeverRepliedLast(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.Threads["t1"] = &Thread{ID: "t1", Path: "a.rs", Comments: []*Comment{
		cmt("c1", "bob", "the cancel_all caveat misses a third caller", false),
		cmt("c2", "alice", "both halves verified and taken", false),
	}}

	d := Diff(snapWith(prev), snapWith(next), "me")
	ev := d.Events[0]
	if ev.Kind != EvThreadNew {
		t.Fatalf("want thread_new, got %v", kinds(d))
	}
	if ev.Author != "bob" {
		t.Fatalf("thread_new credited %q, want bob (whoever opened it)", ev.Author)
	}
	if ev.Detail != "the cancel_all caveat misses a third caller" {
		t.Fatalf("thread_new quoted %q, want the opening comment", ev.Detail)
	}
}

func TestMergedAndClosedWake(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.State = "MERGED"
	d := Diff(snapWith(prev), snapWith(next), "me")
	if tierOf(d, EvPRMerged) != TierWake {
		t.Fatalf("merge did not wake: %v", kinds(d))
	}
}

func TestIdenticalSnapshotsProduceNothing(t *testing.T) {
	// Stability matters more than it looks: an unstable ordering or a
	// recomputed digest makes an unchanged PR look changed and burns a turn
	// on every single poll, forever.
	pr := basePR()
	pr.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "x", false)}}
	pr2 := basePR()
	pr2.Threads["t1"] = &Thread{ID: "t1", Comments: []*Comment{cmt("c1", "alice", "x", false)}}
	d := Diff(snapWith(pr), snapWith(pr2), "me")
	if !d.Empty() {
		t.Fatalf("identical snapshots produced %d events: %v", len(d.Events), kinds(d))
	}
	if d.Summary() != "no change" {
		t.Fatalf("summary = %q", d.Summary())
	}
}

func TestMergeEventsDeduplicatesAndCaps(t *testing.T) {
	e := Event{Kind: EvCommentNew, PR: "acme/widget#1", ThreadID: "t", Comment: "c"}
	got := mergeEvents([]Event{e}, []Event{e, e})
	if len(got) != 1 {
		t.Fatalf("dedup failed: %d events", len(got))
	}
	var many []Event
	for i := 0; i < 500; i++ {
		many = append(many, Event{Kind: EvCommentNew, Comment: string(rune(i))})
	}
	if n := len(mergeEvents(nil, many)); n > 200 {
		t.Fatalf("pending batch grew unbounded: %d", n)
	}
}

// Observed live in production use: one reviewer's single review
// action produced NINE distinct GraphQL review nodes on the same PR by the
// same author (one CHANGES_REQUESTED plus eight COMMENTED, one per inline
// thread). EvReviewSubmitted carried no field that distinguished them --
// same Kind, same PR, same empty ThreadID/Comment, same Detail ("COMMENTED")
// -- so mergeEvents' dedup key collapsed all eight COMMENTED submissions
// into one. Seven real review events vanished with no trace: not elided
// (the elide counter only fires past the 12-event projection cap), not
// suppressed, just gone before the delta was even built.
func TestDistinctReviewsWithSameStateAreNotDeduplicated(t *testing.T) {
	var fresh []Event
	for i := 0; i < 8; i++ {
		fresh = append(fresh, Event{
			Kind: EvReviewSubmitted, Tier: TierRecord, PR: "acme/widget#23654",
			Author: "bob", Detail: "COMMENTED", ReviewID: string(rune('a' + i)),
		})
	}
	got := mergeEvents(nil, fresh)
	if len(got) != 8 {
		t.Fatalf("8 distinct reviews collapsed to %d: %v", len(got), got)
	}
}

// Observed live in production use: alice has held a PENDING
// review (id PRR_kwDOAAZdB84KiCIQ) since before adoption. A PENDING review
// is a draft, invisible to anyone else, until its author clicks "submit" --
// at which point GitHub flips its State in place; the ID never changes.
// The review-diff loop kept every review it had ever seen in a
// "seen this ID" set and only ever fired on a brand-new ID, so a PENDING
// review adopted once and submitted for real on a later round produced
// zero events, forever: the false absence a quiet monitor is built to have.
func TestPendingReviewSubmittedLaterWakes(t *testing.T) {
	prev := basePR()
	prev.Reviews["r1"] = &Review{ID: "r1", Author: "alice", State: "PENDING"}
	next := basePR()
	next.Reviews["r1"] = &Review{ID: "r1", Author: "alice", State: "CHANGES_REQUESTED", SubmittedAt: "later"}

	d := Diff(snapWith(prev), snapWith(next), "me")
	if !d.WakeWorthy() {
		t.Fatalf("a PENDING review submitted for real must wake; got %v", kinds(d))
	}
	if tierOf(d, EvReviewSubmitted) != TierWake {
		t.Fatalf("review_submitted tier = %d, want %d", tierOf(d, EvReviewSubmitted), TierWake)
	}
	// A truly identical review (same ID, same state) on the next round must
	// not re-fire: this is the negative control for the fix above.
	same := Diff(snapWith(next), snapWith(next), "me")
	if same.WakeWorthy() {
		t.Fatalf("an unchanged review re-fired: %v", kinds(same))
	}
}

// Observed live in production use: dangau's review PRR_...Ktfe8
// was submitted APPROVED at 06:56:14Z and dismissed by alice's next push
// twelve seconds later; ebac's poll gap straddled both, so the review-diff
// loop saw the ID for the first time already holding State "DISMISSED".
// Detail fell back to the bare state ("DISMISSED") with no "->", identical
// in shape to a review that was somehow *submitted* pre-dismissed -- which
// GitHub's API cannot produce; DISMISSED is only ever reached by dismissing
// an existing APPROVED or CHANGES_REQUESTED review. Read on its own the line
// read "review_submitted by dangau — DISMISSED", which claims a fresh
// submission that never happened and omits the real one (an approval) that
// did. The event must say so was collapsed rather than assert a fiction.
func TestReviewBornDismissedFlagsTheCollapse(t *testing.T) {
	prev := basePR()
	next := basePR()
	next.Reviews["r1"] = &Review{ID: "r1", Author: "dangau", State: "DISMISSED", SubmittedAt: "t1"}

	d := Diff(snapWith(prev), snapWith(next), "me")
	if !d.WakeWorthy() {
		t.Fatalf("a collapsed approve-then-dismiss must still wake: %v", kinds(d))
	}
	var ev *Event
	for i := range d.Events {
		if d.Events[i].Kind == EvReviewSubmitted {
			ev = &d.Events[i]
		}
	}
	if ev == nil {
		t.Fatalf("no review_submitted event: %v", kinds(d))
	}
	if ev.Detail == "DISMISSED" {
		t.Fatalf("detail %q asserts a submission GitHub cannot produce; must flag the collapse", ev.Detail)
	}
	if !strings.Contains(ev.Detail, "DISMISSED") {
		t.Fatalf("detail %q dropped the actual end state", ev.Detail)
	}

	// A review that really is new and non-terminal (e.g. a first-ever
	// COMMENTED) must NOT be relabeled: this is the negative control.
	next2 := basePR()
	next2.Reviews["r2"] = &Review{ID: "r2", Author: "dangau", State: "COMMENTED", SubmittedAt: "t1", Body: "looks fine"}
	d2 := Diff(snapWith(prev), snapWith(next2), "me")
	if kinds(d2)[0] != string(EvReviewSubmitted) {
		t.Fatalf("want review_submitted, got %v", kinds(d2))
	}
	if d2.Events[0].Detail != "COMMENTED" {
		t.Fatalf("an ordinary first submission got relabeled: %q", d2.Events[0].Detail)
	}
}

// Observed live in production use: dangau (an automated
// reviewer, "aether-review-bot:v1") holds one review ID across the whole
// PR and EDITS ITS BODY IN PLACE as new commits land, while both State
// (CHANGES_REQUESTED) and the pinned Commit field never change. Round 1184's
// blocking defect was "Identifiers logged without scrubbing"; by round 1190,
// same ID, same state, same commit-of-record, the body had become a wholly
// different blocking defect ("Tests assert against struct Debug format").
// The review-diff loop only ever compared State, so a reviewer silently
// swapping one blocking finding for another produced zero events -- the
// false absence a quiet monitor is built to have, and worse than most: the
// operator would see "still CHANGES_REQUESTED" and never learn the actual
// objection changed underneath that label.
func TestReviewEditedInPlaceStillWakes(t *testing.T) {
	prev := basePR()
	prev.Reviews["r1"] = &Review{
		ID: "r1", Author: "dangau", State: "CHANGES_REQUESTED", SubmittedAt: "t1",
		Body: "Blocking: identifiers logged without scrubbing.",
	}
	next := basePR()
	next.Reviews["r1"] = &Review{
		ID: "r1", Author: "dangau", State: "CHANGES_REQUESTED", SubmittedAt: "t1",
		Body: "Blocking: tests assert against struct Debug format instead of emitted telemetry.",
	}

	d := Diff(snapWith(prev), snapWith(next), "me")
	if !d.WakeWorthy() {
		t.Fatalf("a review whose blocking finding changed underneath an unchanged state must wake: %v", kinds(d))
	}

	// Negative control: truly nothing changed (same state, same body) must
	// not re-fire on every poll forever.
	same := Diff(snapWith(next), snapWith(next), "me")
	if same.WakeWorthy() {
		t.Fatalf("an unchanged review re-fired: %v", kinds(same))
	}

	// Negative control: our own review being edited (e.g. alice revising
	// a reply) must not wake -- same rule as every other self-authored path.
	prevMe := basePR()
	prevMe.Reviews["r2"] = &Review{ID: "r2", Author: "me", State: "COMMENTED", SubmittedAt: "t1", Body: "first take"}
	nextMe := basePR()
	nextMe.Reviews["r2"] = &Review{ID: "r2", Author: "me", State: "COMMENTED", SubmittedAt: "t1", Body: "revised take"}
	dMe := Diff(snapWith(prevMe), snapWith(nextMe), "me")
	if dMe.WakeWorthy() {
		t.Fatalf("our own edited review woke us: %v", kinds(dMe))
	}
}

func TestBackoffClimbsAndCaps(t *testing.T) {
	if backoffFor(1) != baseBackoff {
		t.Fatalf("first retry = %s", backoffFor(1))
	}
	if backoffFor(2) != 2*baseBackoff {
		t.Fatalf("second retry = %s", backoffFor(2))
	}
	if backoffFor(20) != maxBackoff {
		t.Fatalf("backoff did not cap: %s", backoffFor(20))
	}
}

func TestParsePRRef(t *testing.T) {
	for _, tc := range []struct {
		in    string
		owner string
		repo  string
		num   int
		bad   bool
	}{
		{in: "https://github.com/acme/widget/pull/6622", owner: "acme", repo: "widget", num: 6622},
		{in: "acme/widget#42", owner: "acme", repo: "widget", num: 42},
		{in: "https://github.com/acme/widget/issues/12", bad: true},
		{in: "widget#1", bad: true},
		{in: "", bad: true},
	} {
		got, err := ParsePRRef(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q should not parse, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Owner != tc.owner || got.Repo != tc.repo || got.Number != tc.num {
			t.Errorf("%q -> %+v", tc.in, got)
		}
	}
}

func TestIsBot(t *testing.T) {
	for _, tc := range []struct {
		a    gqlAuthor
		want bool
	}{
		{gqlAuthor{Login: "copilot-pull-request-reviewer", Typename: "Bot"}, true},
		{gqlAuthor{Login: "some-app[bot]", Typename: "User"}, true},
		{gqlAuthor{Login: "coderabbitai", Typename: "User"}, true},
		{gqlAuthor{Login: "alice", Typename: "User"}, false},
	} {
		if got := isBot(tc.a); got != tc.want {
			t.Errorf("isBot(%s) = %v, want %v", tc.a.Login, got, tc.want)
		}
	}
}

// The form is mirrored whole into a model's context, so a runaway projection
// is a real cost, not a cosmetic one.
func TestDeltaProjectionIsBounded(t *testing.T) {
	var events []Event
	for i := 0; i < 100; i++ {
		events = append(events, Event{Kind: EvCommentNew, Tier: TierWake, PR: "acme/widget#1",
			Detail: strings.Repeat("x", 400)})
	}
	p := projectDelta(3, events, true, time.Now(), 0)
	if len(p.Events) > maxProjectedEvents {
		t.Fatalf("projected %d event lines, cap is %d", len(p.Events), maxProjectedEvents)
	}
	if p.Elided != 100-maxProjectedEvents {
		t.Fatalf("elided count = %d, want %d", p.Elided, 100-maxProjectedEvents)
	}
	for _, line := range p.Events {
		if len([]rune(line)) > 300 {
			t.Fatalf("projected line too long: %d runes", len([]rune(line)))
		}
	}
}

func TestFirstLineTrimsOnRuneBoundary(t *testing.T) {
	in := strings.Repeat("é", 400)
	got := firstLine(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("no ellipsis: %q", got[len(got)-8:])
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatal("trimmed mid-rune: replacement character in output")
	}
}

func TestSetJSONRefusesDottedKeys(t *testing.T) {
	// Guards the measured figaro hazard: a dotted write into a subtree that
	// was written as a literal creates a flat decoy key, and a later unset
	// "succeeds" against the decoy while the real value stands.
	f := NewFigaro("/nonexistent-figaro")
	err := f.SetJSON(t.Context(), "@x", "prs.foo", 1)
	if err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("dotted key was not refused: %v", err)
	}
}

func TestValidNameRejectsSystemdHostileNames(t *testing.T) {
	for _, bad := range []string{"", "a b", "a/b", "a.b", strings.Repeat("x", 65)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
	for _, good := range []string{"widget", "aether-prs", "pr_6622"} {
		if err := ValidName(good); err != nil {
			t.Errorf("ValidName(%q): %v", good, err)
		}
	}
}

// Regression: the roster's health column scored an unarmed critic as "ok"
// because it compared TimerState against the literal "inactive/disabled",
// while systemd actually says "not-found" for a unit that was never
// installed. The selfcheck disagreed with the roster about one fact. Health
// must be derived from the PROPERTY, so no unrecognised display string can
// ever read as healthy.
func TestUnarmedTimerIsNeverHealthy(t *testing.T) {
	w := &Critic{Name: "x"}
	row := RosterRow{Seat: "abc123"}
	for _, ts := range []TimerStatus{
		{Display: "absent"},
		{Display: "disabled", Known: true},
		{Display: "enabled/inactive", Known: true, Enabled: true},
		{Display: "something-systemd-invents-in-2027"},
	} {
		if got := health(w, row, ts); got == "ok" {
			t.Errorf("timer %+v scored healthy", ts)
		}
	}
	armed := TimerStatus{Active: true, Enabled: true, Known: true, Display: "armed"}
	if got := health(w, row, armed); got != "ok" {
		t.Errorf("armed timer scored %q, want ok", got)
	}
}

func TestUnreachableSeatIsNeverHealthy(t *testing.T) {
	armed := TimerStatus{Active: true, Enabled: true, Known: true, Display: "armed"}
	for _, seat := range []string{"(empty)", "abc123 (unreachable)"} {
		if got := health(&Critic{Name: "x"}, RosterRow{Seat: seat}, armed); got == "ok" {
			t.Errorf("seat %q scored healthy", seat)
		}
	}
}

func TestValidAriaID(t *testing.T) {
	// `figaro bind -j` returns figaro_id, `figaro new -j` returns aria_id.
	// A script that reads the wrong field yields the literal "null", and
	// casting that produced an opaque xwal error. Refuse it up front.
	for _, bad := range []string{"", "null", "@1cb8a917", "not an id", "<nil>"} {
		if err := ValidAriaID(bad); err == nil {
			t.Errorf("ValidAriaID(%q) accepted", bad)
		}
	}
	for _, good := range []string{"0a0db7ec", "f37a50e5"} {
		if err := ValidAriaID(good); err != nil {
			t.Errorf("ValidAriaID(%q): %v", good, err)
		}
	}
}

// Succession thresholds ship in the binary, so they are testable without a
// config file present — which is the whole point of moving them here.
func TestSuccessionPolicyIsShippedAndOrdered(t *testing.T) {
	p := DefaultSuccession()
	if p.StandbyAt >= p.RecastAt {
		t.Fatalf("standby %.2f must come before recast %.2f", p.StandbyAt, p.RecastAt)
	}
	if p.StandbyAt != 0.70 || p.RecastAt != 0.80 {
		t.Fatalf("thresholds drifted: %.2f/%.2f", p.StandbyAt, p.RecastAt)
	}
	if p.ForkTurn > 2 {
		t.Fatalf("fork turn %d is not early; a late fork inherits the fill it is meant to escape", p.ForkTurn)
	}
	if len(p.DivisibleTasks) == 0 {
		t.Fatal("standby has no read-only warm-up tasks")
	}
	for _, task := range p.DivisibleTasks {
		l := strings.ToLower(task)
		if strings.Contains(l, "reply to") && !strings.Contains(l, "do not") {
			t.Fatalf("standby task lets a second holder act: %q", task)
		}
	}
}

// An unknown context_limit must not read as an empty context. Reporting 0%
// for a seat that may be full is the direction that loses the assignment.
func TestUnknownContextLimitIsAnErrorNotZeroFill(t *testing.T) {
	f := NewFigaro("/nonexistent-figaro")
	_, _, _, err := f.AriaFill(t.Context(), "deadbeef")
	if err == nil {
		t.Fatal("a failed status read reported a usable fill")
	}
}

func TestSuccessionDecision(t *testing.T) {
	p := DefaultSuccession()
	for _, tc := range []struct {
		name         string
		fill         float64
		standby      string
		standbyAlive bool
		want         succAction
	}{
		{"idle", 0.34, "", false, succNone},
		{"just below standby", 0.699, "", false, succNone},
		{"at standby", 0.70, "", false, succMint},
		{"above standby, standby exists", 0.75, "abc123", true, succNone},
		{"at recast with a live standby", 0.80, "abc123", true, succRecast},
		// The one that would otherwise hang forever: full, but the standby
		// was reaped. Re-mint rather than wait for an aria that is gone.
		{"at recast, standby reaped", 0.85, "abc123", false, succRemint},
		// And full with no standby at all still mints one rather than
		// falling through to none.
		{"at recast, no standby", 0.85, "", false, succMint},
	} {
		if got := successionAction(tc.fill, tc.standby, tc.standbyAlive, p); got != tc.want {
			t.Errorf("%s: fill=%.3f standby=%q alive=%v → %s, want %s",
				tc.name, tc.fill, tc.standby, tc.standbyAlive, got, tc.want)
		}
	}
}
