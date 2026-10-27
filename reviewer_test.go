package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ebacbrief "github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/source/github"
)

func TestAsReviewerWakesOnPushAndOwnDismissal(t *testing.T) {
	const me = "jack"
	for _, tc := range []struct {
		name string
		ev   model.Event
		want model.Tier
	}{
		{"push", model.Event{Kind: model.EvHeadMoved, Tier: model.TierRecord}, model.TierWake},
		{"our approval dismissed", model.Event{Kind: model.EvReviewSubmitted, Tier: model.TierRecord, Author: me, Detail: "APPROVED -> DISMISSED"}, model.TierWake},
		{"our approval submitted", model.Event{Kind: model.EvReviewSubmitted, Tier: model.TierRecord, Author: me, Detail: "APPROVED"}, model.TierRecord},
		{"someone else dismissed", model.Event{Kind: model.EvReviewSubmitted, Tier: model.TierRecord, Author: "bot", Detail: "CHANGES_REQUESTED -> DISMISSED"}, model.TierRecord},
		{"thread resolved", model.Event{Kind: model.EvThreadResolved, Tier: model.TierRecord}, model.TierRecord},
	} {
		d := &model.Delta{Events: []model.Event{tc.ev}}
		github.AsReviewer(d, me)
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

	theirs := &model.PRState{Key: model.MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend"}
	mine := &model.PRState{Key: model.MakePRKey("acme", "widget", 8), Number: 8, State: "OPEN", Author: "jack"}
	w := &model.Critic{Name: "rev", Mode: model.ModeReviewer, Host: "ghe.example", Approve: true,
		Worktree: &model.Worktree{Clone: "/src/widget", Root: "/wt"}, Notes: "skill://widget-review"}
	brief := ebacbrief.ProtocolText(w, snapWith(theirs, mine), "jack")

	for _, want := range []string{
		"You may APPROVE: acme/widget#7.\n",
		"worktree add --detach /wt/rev-pr7 FETCH_HEAD",
		"Read skill://widget-review first",
		"ebac post --critic rev --pr <owner/repo#n> --event COMMENT|APPROVE",
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
	if !strings.Contains(ebacbrief.ProtocolText(w, snapWith(theirs), "jack"), "NO SIGNATURE") {
		t.Error("a missing signature must stop the seat posting")
	}
}

func TestHarnessAuthorityFollowsTheGrant(t *testing.T) {
	for _, tc := range []struct {
		critic model.Critic
		prefix string
	}{
		{model.Critic{Mode: model.ModeReview}, "READ-ONLY"},
		{model.Critic{Mode: model.ModeReview, Write: true}, "WRITE"},
		{model.Critic{Mode: model.ModeReviewer}, "REVIEWER: comment and reply."},
		{model.Critic{Mode: model.ModeReviewer, Approve: true}, "REVIEWER: comment, reply, and approve"},
	} {
		if got := ebacbrief.NewHarnessContract(&tc.critic).Authority; !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("%+v: authority %q, want prefix %q", tc.critic.Mode, got, tc.prefix)
		}
	}
}

func TestMarkStamped(t *testing.T) {
	bot := &model.Review{ID: "b", Author: "automaton[bot]", IsBot: true, State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-07T21:50:25Z"}
	for _, tc := range []struct {
		name string
		r    model.Review
		want bool
	}{
		{"body marker", model.Review{State: "COMMENTED", SubmittedAt: "2026-10-07T10:00:00Z", Body: "x. No person reviewed this change; y"}, true},
		{"same state 5s after the bot", model.Review{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-07T21:50:30Z"}, true},
		{"same state 2m after the bot", model.Review{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-07T21:52:30Z"}, false},
		{"other state 5s after the bot", model.Review{State: "APPROVED", SubmittedAt: "2026-10-07T21:50:30Z"}, false},
		{"same state before the bot", model.Review{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-07T21:50:20Z"}, false},
	} {
		r := tc.r
		r.ID, r.Author = "h", "lukas"
		forge.MarkStamped(map[string]*model.Review{"b": bot, "h": &r})
		if r.Stamped != tc.want || bot.Stamped {
			t.Errorf("%s: stamped %v (bot %v), want %v", tc.name, r.Stamped, bot.Stamped, tc.want)
		}
	}
}

func TestStampedReviewStillWakesAndSaysSo(t *testing.T) {
	stamped := &model.Review{ID: "h", Author: "lukas", State: "CHANGES_REQUESTED", Stamped: true}
	pr := func(rs ...*model.Review) *model.PRState {
		p := &model.PRState{Key: model.MakePRKey("acme", "widget", 1), State: "OPEN", Reviews: map[string]*model.Review{}}
		for _, r := range rs {
			p.Reviews[r.ID] = r
		}
		return p
	}
	d := github.Diff(snapWith(pr()), snapWith(pr(stamped)), "jack")
	if len(d.Events) != 1 || d.Events[0].Tier != model.TierWake || !strings.Contains(d.Events[0].Line(), "by lukas (bot-stamped") {
		t.Fatalf("events %+v", d.Events)
	}
}

func TestNewThreadTierByOpener(t *testing.T) {
	for _, tc := range []struct {
		opener string
		isBot  bool
		want   model.Tier
	}{
		{"John-Kelliher", false, model.TierRecord},
		{"john-kelliher", false, model.TierRecord},
		{"slahoti", false, model.TierWake},
		{"automaton[bot]", true, model.TierRecord},
	} {
		th := &model.Thread{ID: "t", Comments: []*model.Comment{{Author: tc.opener, IsBot: tc.isBot, Body: "x"}}}
		prev := snapWith(&model.PRState{Key: model.MakePRKey("acme", "widget", 1), State: "OPEN"})
		next := snapWith(&model.PRState{Key: model.MakePRKey("acme", "widget", 1), State: "OPEN", Threads: map[string]*model.Thread{"t": th}})
		d := github.Diff(prev, next, "John-Kelliher")
		if len(d.Events) != 1 || d.Events[0].Tier != tc.want {
			t.Errorf("opener %s: events %+v, want tier %d", tc.opener, d.Events, tc.want)
		}
	}
}

func TestPostingSeatsAreToldToSignalNotNarrate(t *testing.T) {
	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	pr := &model.PRState{Key: model.MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend"}
	for _, tc := range []struct {
		critic model.Critic
		want   bool
	}{
		{model.Critic{Name: "ro", Mode: model.ModeReview}, false},
		{model.Critic{Name: "rw", Mode: model.ModeReview, Write: true}, true},
		{model.Critic{Name: "rev", Mode: model.ModeReviewer, Host: "ghe.example"}, true},
	} {
		brief := ebacbrief.ProtocolText(&tc.critic, snapWith(pr), "jack")
		if got := strings.Contains(brief, "SIGNAL, DO NOT NARRATE"); got != tc.want {
			t.Errorf("%s: signal section present=%v, want %v", tc.critic.Name, got, tc.want)
		}
	}
}
