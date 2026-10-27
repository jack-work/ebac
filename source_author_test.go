package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
)

func apr(n int, state string, draft bool, head string) forge.AuthoredPR {
	return forge.AuthoredPR{Repo: "bic/aether", Number: n, URL: "https://x/pull/" + head, Title: "t", State: state, Draft: draft, Head: head}
}

func authorKinds(ev []model.Event) map[string]model.Tier {
	m := map[string]model.Tier{}
	for _, e := range ev {
		m[string(e.PR)+" "+string(e.Kind)] = e.Tier
	}
	return m
}

func TestAuthorDiff(t *testing.T) {
	st := &model.AuthorState{}
	if ev := authorDiff(st, []forge.AuthoredPR{apr(1, "OPEN", false, "a"), apr(2, "OPEN", true, "b"), apr(3, "MERGED", false, "c")}); len(ev) != 0 {
		t.Fatalf("first round must only record: %+v", ev)
	}
	ev := authorDiff(st, []forge.AuthoredPR{
		apr(1, "MERGED", false, "a"), // merged -> wake
		apr(2, "OPEN", false, "b"),   // draft turned ready -> wake new
		apr(3, "MERGED", false, "c"), // unchanged
		apr(4, "OPEN", false, "d"),   // new -> wake
		apr(5, "OPEN", true, "e"),    // new draft -> record
		apr(6, "MERGED", false, "f"), // opened and merged between rounds -> wake merged
	})
	got := authorKinds(ev)
	want := map[string]model.Tier{
		"bic/aether#1 author_pr_merged": model.TierWake,
		"bic/aether#2 author_pr_new":    model.TierWake,
		"bic/aether#4 author_pr_new":    model.TierWake,
		"bic/aether#5 author_pr_draft":  model.TierRecord,
		"bic/aether#6 author_pr_merged": model.TierWake,
	}
	if len(got) != len(want) {
		t.Fatalf("events %v; want %v", got, want)
	}
	for k, tier := range want {
		if got[k] != tier {
			t.Fatalf("%s: tier %v; all %v", k, got[k], got)
		}
	}
	ev = authorDiff(st, []forge.AuthoredPR{
		apr(1, "MERGED", false, "a"), apr(2, "OPEN", false, "b2"), apr(3, "MERGED", false, "c"),
		apr(4, "CLOSED", false, "d"), apr(5, "OPEN", true, "e"), apr(6, "MERGED", false, "f"),
	})
	got = authorKinds(ev)
	if got["bic/aether#2 author_pr_head_moved"] != model.TierRecord || got["bic/aether#4 author_pr_closed"] != model.TierRecord || len(got) != 2 {
		t.Fatalf("head move + close must be recorded only: %v", got)
	}
	if (&model.Delta{Events: ev}).WakeWorthy() {
		t.Fatal("a round of closes and head moves woke the seat")
	}
	ev = authorDiff(st, []forge.AuthoredPR{apr(4, "OPEN", false, "d")})
	if authorKinds(ev)["bic/aether#4 author_pr_reopened"] != model.TierRecord {
		t.Fatalf("reopen: %v", authorKinds(ev))
	}
}

func TestListAuthoredPRsArgsAndParse(t *testing.T) {
	t.Setenv("EBAC_FAKE_LOG", t.TempDir()+"/calls")
	g := fakeGH(t, "author")
	since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	prs, err := g.ListAuthoredPRs(context.Background(), "bic/aether", "Chiqun-Zhang", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].Head != "ffff8f68aaaa" || prs[1].State != "MERGED" || prs[1].Merged == "" || prs[0].Merged != "" {
		t.Fatalf("parsed %+v", prs)
	}
	b, _ := os.ReadFile(os.Getenv("EBAC_FAKE_LOG"))
	args := string(b)
	for _, want := range []string{"--author Chiqun-Zhang", "--state all", "updated:>=2026-10-15T00:00:00Z", "--repo bic/aether"} {
		if !strings.Contains(args, want) {
			t.Fatalf("gh args lack %q: %s", want, args)
		}
	}
}

func TestAuthorBrief(t *testing.T) {
	w := &model.Critic{Name: "author-x", FormID: "@f", Author: &model.AuthorRef{Login: "Chiqun-Zhang"}}
	w.Runtime.Round = 7
	b := authorBrief(w, []model.Event{
		{Kind: EvAuthorNew, Tier: model.TierWake, PR: "bic/aether#4", URL: "u4", Detail: "new one"},
		{Kind: EvAuthorMerged, Tier: model.TierWake, PR: "bic/aether#1", URL: "u1", Detail: "merged one"},
		{Kind: EvAuthorHead, Tier: model.TierRecord, PR: "bic/aether#2"},
	})
	for _, want := range []string{"- NEW bic/aether#4 u4 new one", "- MERGED bic/aether#1 u1 merged one", "(1 background change(s)", "ebac ack --critic author-x --round 7"} {
		if !strings.Contains(b, want) {
			t.Fatalf("brief lacks %q:\n%s", want, b)
		}
	}
}

func TestLiveAuthorWatchRoundsAndWindowEnd(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-author")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-author-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeAuthor, Source: "github-author",
		Host: "microsoft.ghe.com", CreatedAt: time.Now().UTC(), FormID: id,
		Author: &model.AuthorRef{Login: "Chiqun-Zhang", Since: time.Now().Add(-time.Hour), Repos: []string{"bic/aether"}}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	gh := fakeGH(t, "author")
	gh.Host = "github.com" // the poller's default: the round must retarget it to the critic's host
	p := &Poller{Store: st, Fig: f, Out: &out, GH: gh}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v", err)
	}
	if gh.Host != "microsoft.ghe.com" {
		t.Fatalf("round ran against %s, not the critic's host", gh.Host)
	}
	form, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(form["authored"])
	if !strings.Contains(string(raw), `"bic/aether#61586"`) || !strings.Contains(string(raw), `"MERGED"`) {
		t.Fatalf("authored key: %s", raw)
	}
	c, _ := st.LoadCritic(ctx, w.Name)
	if len(c.Runtime.Author.Seen) != 2 || len(c.Runtime.PendingEvents) != 0 {
		t.Fatalf("baseline: seen %d pending %d", len(c.Runtime.Author.Seen), len(c.Runtime.PendingEvents))
	}
	past := time.Now().Add(-time.Minute)
	c.Author.Until = &past
	if err := st.SaveCritic(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.LoadCritic(ctx, w.Name); !c.Runtime.Stopped || !strings.Contains(c.Runtime.StopReason, "window ended") {
		t.Fatalf("not stopped at the window end: %+v", c.Runtime)
	}
}
