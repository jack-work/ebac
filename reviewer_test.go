package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAsReviewerWakesOnPushAndOwnDismissal(t *testing.T) {
	const me = "jack"
	for _, tc := range []struct {
		name string
		ev   Event
		want Tier
	}{
		{"push", Event{Kind: EvHeadMoved, Tier: TierRecord}, TierWake},
		{"our approval dismissed", Event{Kind: EvReviewSubmitted, Tier: TierRecord, Author: me, Detail: "APPROVED -> DISMISSED"}, TierWake},
		{"our approval submitted", Event{Kind: EvReviewSubmitted, Tier: TierRecord, Author: me, Detail: "APPROVED"}, TierRecord},
		{"someone else dismissed", Event{Kind: EvReviewSubmitted, Tier: TierRecord, Author: "bot", Detail: "CHANGES_REQUESTED -> DISMISSED"}, TierRecord},
		{"thread resolved", Event{Kind: EvThreadResolved, Tier: TierRecord}, TierRecord},
	} {
		d := &Delta{Events: []Event{tc.ev}}
		asReviewer(d, me)
		if got := d.Events[0].Tier; got != tc.want {
			t.Errorf("%s: tier %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestReviewerBrief(t *testing.T) {
	sig := filepath.Join(t.TempDir(), "signature")
	if err := os.WriteFile(sig, []byte("-- signed, the reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EBAC_SIGNATURE", sig)

	theirs := &PRState{Key: MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend"}
	mine := &PRState{Key: MakePRKey("acme", "widget", 8), Number: 8, State: "OPEN", Author: "jack"}
	w := &Critic{Name: "rev", Mode: ModeReviewer, Host: "ghe.example", Approve: true,
		Worktree: &Worktree{Clone: "/src/widget", Root: "/wt"}, Notes: "skill://widget-review"}
	brief := buildBrief(w, snapWith(theirs, mine), nil, "jack", 0)

	for _, want := range []string{
		"You may APPROVE: acme/widget#7.\n",
		"worktree add --detach /wt/rev-pr7 FETCH_HEAD",
		"Read skill://widget-review first",
		"GH_HOST=ghe.example gh api",
		"SIGNATURE\n-- signed, the reviewer\n",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if strings.Contains(brief, "READ-ONLY") {
		t.Error("reviewer brief carries the read-only boundary")
	}

	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	if !strings.Contains(buildBrief(w, snapWith(theirs), nil, "jack", 0), "NO SIGNATURE") {
		t.Error("a missing signature must stop the seat posting")
	}
}

func TestHarnessAuthorityFollowsTheGrant(t *testing.T) {
	for _, tc := range []struct {
		critic Critic
		prefix string
	}{
		{Critic{Mode: ModeReview}, "READ-ONLY"},
		{Critic{Mode: ModeReview, Write: true}, "WRITE"},
		{Critic{Mode: ModeReviewer}, "REVIEWER: comment and reply."},
		{Critic{Mode: ModeReviewer, Approve: true}, "REVIEWER: comment, reply, and approve"},
	} {
		if got := harnessContract(&tc.critic).Authority; !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("%+v: authority %q, want prefix %q", tc.critic.Mode, got, tc.prefix)
		}
	}
}
