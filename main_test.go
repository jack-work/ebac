package main

import "testing"

// TestPRTableRowsAwaitingUsesRealIdentity reproduces the bug where `ebac
// show` always echoed UNRES into the AWAITING column: cmdShow used to call
// p.Counts("") instead of the identity poll.go resolves via GH.Whoami, and
// AwaitingUs(me) with me="" is true for any thread whose last comment has a
// non-empty author -- i.e. always. Measured live on 2026-09-26: the form
// (built with the real "me") showed bic/aether#30471 at unresolved=12,
// awaiting_reply=11, while `ebac show`'s table showed AWAITING=12, silently
// claiming the seat's own already-answered thread was still waiting on it.
//
// This test builds a thread the seat itself answered last (not resolved,
// but the last word was "figaro-bot"'s) and checks that passing the real
// identity reports it as NOT awaiting, matching what the form already got
// right and what the old hardcoded "" call site got wrong.
func TestPRTableRowsAwaitingUsesRealIdentity(t *testing.T) {
	const me = "figaro-bot"
	pr := &PRState{
		Key:     MakePRKey("bic", "aether", 30471),
		State:   "OPEN",
		HeadSHA: "c8a67a72",
		Title:   "Split atomic instructions",
		Threads: map[string]*Thread{
			"t1": { // unresolved, but WE answered last -> not awaiting us
				ID:         "t1",
				IsResolved: false,
				Comments: []*Comment{
					{Author: "a-human", Body: "question"},
					{Author: me, Body: "answered"},
				},
			},
			"t2": { // unresolved, human answered last -> awaiting us
				ID:         "t2",
				IsResolved: false,
				Comments: []*Comment{
					{Author: me, Body: "answered"},
					{Author: "a-human", Body: "one more thing"},
				},
			},
		},
	}
	snap := &Snapshot{PRs: map[PRKey]*PRState{pr.Key: pr}}
	refs := []PRRef{{Owner: "bic", Repo: "aether", Number: 30471, Title: pr.Title}}

	rows := prTableRows(refs, snap, me)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Unresolved != 2 {
		t.Fatalf("Unresolved = %d, want 2 (both threads open)", row.Unresolved)
	}
	if row.Awaiting != 1 {
		t.Fatalf("Awaiting = %d, want 1 (only t2 awaits a reply from %q); "+
			"got Unresolved == Awaiting which is the me=%q bug reproduced", row.Awaiting, me, "")
	}

	// The old call site's exact bug, held up as the negative control: with
	// me="" every non-empty last-author counts as "not me", so this must
	// come back 2 (== Unresolved), not 1.
	buggyRows := prTableRows(refs, snap, "")
	if buggyRows[0].Awaiting != 2 {
		t.Fatalf("sanity check on the reproduction itself failed: with me=\"\" got Awaiting=%d, want 2", buggyRows[0].Awaiting)
	}
}
