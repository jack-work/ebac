package author

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
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
	if ev := authorDiff(st, []forge.AuthoredPR{apr(1, "OPEN", false, "a"), apr(2, "OPEN", true, "b"), apr(3, "MERGED", false, "c")}, time.Time{}); len(ev) != 0 {
		t.Fatalf("first round must only record: %+v", ev)
	}
	ev := authorDiff(st, []forge.AuthoredPR{
		apr(1, "MERGED", false, "a"), // merged -> wake
		apr(2, "OPEN", false, "b"),   // draft turned ready -> wake new
		apr(3, "MERGED", false, "c"), // unchanged
		apr(4, "OPEN", false, "d"),   // new -> wake
		apr(5, "OPEN", true, "e"),    // new draft -> record
		apr(6, "MERGED", false, "f"), // opened and merged between rounds -> wake merged
	}, time.Time{})
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
	}, time.Time{})
	got = authorKinds(ev)
	if got["bic/aether#2 author_pr_head_moved"] != model.TierRecord || got["bic/aether#4 author_pr_closed"] != model.TierRecord || len(got) != 2 {
		t.Fatalf("head move + close must be recorded only: %v", got)
	}
	if (&model.Delta{Events: ev}).WakeWorthy() {
		t.Fatal("a round of closes and head moves woke the seat")
	}
	ev = authorDiff(st, []forge.AuthoredPR{apr(4, "OPEN", false, "d")}, time.Time{})
	if authorKinds(ev)["bic/aether#4 author_pr_reopened"] != model.TierRecord {
		t.Fatalf("reopen: %v", authorKinds(ev))
	}
}

func TestListAuthoredPRsArgsAndParse(t *testing.T) {
	t.Setenv("EBAC_FAKE_LOG", t.TempDir()+"/calls")
	g := forgetest.FakeGH(t, "author")
	since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	prs, err := g.ListAuthoredPRs(context.Background(), "bic/aether", "Chiqun-Zhang", since, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].Head != "ffff8f68aaaa" || prs[1].State != "MERGED" || prs[1].Merged == "" || prs[0].Merged != "" {
		t.Fatalf("parsed %+v", prs)
	}
	b, _ := os.ReadFile(os.Getenv("EBAC_FAKE_LOG"))
	args := string(b)
	for _, want := range []string{"--author Chiqun-Zhang", "--state all", "updated:>=2026-10-15T00:00:00Z sort:updated-desc", "--limit 50", "--repo bic/aether"} {
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
	st, err := form.NewStore(f)
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
	gh := forgetest.FakeGH(t, "author")
	gh.Host = "github.com" // the poller's default: the round must retarget it to the critic's host
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": Plugin{GH: gh}.Handle}}
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

// An unseen PR that re-entered the window (updated again, created before the previous good round) is adopted
// silently; one created after it is NEW. Control: with no previous round known, unseen is news (as before).
func TestAuthorDiffReentryIsNotNews(t *testing.T) {
	prev := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	old := apr(10, "OPEN", false, "x")
	old.Created = "2026-09-01T00:00:00Z"
	fresh := apr(11, "OPEN", false, "y")
	fresh.Created = "2026-11-09T12:30:00Z"
	oldMerge := apr(12, "MERGED", false, "z")
	oldMerge.Created, oldMerge.Merged = "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z"
	st := &model.AuthorState{Seen: map[string]model.AuthorSeen{"bic/aether#1": {State: "OPEN"}}}
	got := authorKinds(authorDiff(st, []forge.AuthoredPR{old, fresh, oldMerge}, prev))
	if len(got) != 1 || got["bic/aether#11 author_pr_new"] != model.TierWake {
		t.Fatalf("re-entry: %v; want only #11 new", got)
	}
	if _, ok := st.Seen["bic/aether#10"]; !ok {
		t.Fatal("a re-entered PR must still be adopted into seen")
	}
	st = &model.AuthorState{Seen: map[string]model.AuthorSeen{"bic/aether#1": {State: "OPEN"}}}
	if got := authorKinds(authorDiff(st, []forge.AuthoredPR{old}, time.Time{})); got["bic/aether#10 author_pr_new"] != model.TierWake {
		t.Fatalf("control: with no cutoff an unseen PR is news: %v", got)
	}
}

func authorRound(t *testing.T, w *model.Critic, scenario string) *plugin.Reply {
	t.Helper()
	rep, err := plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, scenario)}.Handle}.Call(context.Background(), "gh",
		plugin.Request{Op: plugin.OpRound, Critic: *w})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func authoredLen(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var m map[string]forge.AuthoredPR
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("authored %s: %v", raw, err)
	}
	return len(m)
}

// author-chiqun: 200 PRs is a window of 50 and no failure; the fetch asks for the newest 50; an unchanged window
// writes no key. A form written before the window existed (200 seen, no digest) is pruned on its first round.
func TestAuthorRecentWindow(t *testing.T) {
	log := t.TempDir() + "/calls"
	t.Setenv("EBAC_FAKE_LOG", log)
	w := &model.Critic{Name: "author-x", FormID: "@x", Source: "github-author", Host: "microsoft.ghe.com",
		Author: &model.AuthorRef{Login: "Chiqun-Zhang", Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Repos: []string{"bic/aether"}}}
	// Before rollout: every PR seen, the old 200-entry form.
	w.Runtime.Author = &model.AuthorState{Seen: map[string]model.AuthorSeen{}}
	for i := 0; i < 200; i++ {
		state := "OPEN"
		if i%3 == 0 {
			state = "MERGED"
		}
		w.Runtime.Author.Seen[fmt.Sprintf("bic/aether#%d", 70000+i)] = model.AuthorSeen{State: state, Head: fmt.Sprintf("%040d", i)[:12]}
	}
	w.Runtime.LastSuccess = time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)

	rep := authorRound(t, w, "author200")
	if rep.Error != "" {
		t.Fatalf("a 200-PR author failed the round: %s", rep.Error)
	}
	if n := authoredLen(t, rep.Set["authored"]); n != model.DefaultAuthorRecent {
		t.Fatalf("authored holds %d PRs; want the newest %d", n, model.DefaultAuthorRecent)
	}
	if !strings.Contains(string(rep.Set["authored"]), `"bic/aether#70000"`) || strings.Contains(string(rep.Set["authored"]), `"bic/aether#70050"`) {
		t.Fatal("the window is not the 50 most recently updated")
	}
	if n := len(rep.Critic.Runtime.Author.Seen); n != model.DefaultAuthorRecent {
		t.Fatalf("seen-state not pruned to the window: %d", n)
	}
	if len(rep.Events) != 0 {
		t.Fatalf("long-known PRs announced as news: %d events", len(rep.Events))
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--limit 50") || !strings.Contains(string(calls), "sort:updated-desc") {
		t.Fatalf("the fetch is not the newest 50: %s", calls)
	}

	// Control: the same window next round writes no key.
	again := authorRound(t, rep.Critic, "author200")
	if _, ok := again.Set["authored"]; ok || again.Error != "" {
		t.Fatalf("an unchanged window wrote authored again (error %q)", again.Error)
	}
	// A configured window is honoured.
	w2 := *rep.Critic
	w2.Author = &model.AuthorRef{Login: "Chiqun-Zhang", Since: w.Author.Since, Repos: []string{"bic/aether"}, Recent: 7}
	if n := authoredLen(t, authorRound(t, &w2, "author200").Set["authored"]); n != 7 {
		t.Fatalf("--recent 7 gave %d", n)
	}
}

// Live, through the core: a form written before the window (authored holds 200 entries, seen 200) is pruned to the
// newest 50 by its first round; the next round leaves the key as it is.
func TestLiveAuthoredFormPrunedOnFirstRound(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-author-prune")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-authprune-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeAuthor, Source: "github-author",
		Host: "microsoft.ghe.com", CreatedAt: time.Now().UTC(), FormID: id,
		Author: &model.AuthorRef{Login: "Chiqun-Zhang", Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Repos: []string{"bic/aether"}}}
	w.Runtime.Author = &model.AuthorState{Seen: map[string]model.AuthorSeen{}}
	old := map[string]forge.AuthoredPR{}
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("bic/aether#%d", 70000+i)
		state := "OPEN"
		if i%3 == 0 {
			state = "MERGED" // as the fixture has it: no transitions, so no news on the first round
		}
		w.Runtime.Author.Seen[k] = model.AuthorSeen{State: state, Head: fmt.Sprintf("%040d", i)[:12]}
		old[k] = forge.AuthoredPR{Repo: "bic/aether", Number: 70000 + i, Title: strings.Repeat("t", 80), State: "OPEN"}
	}
	w.Runtime.LastSuccess = time.Now().UTC()
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(ctx, id, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{"authored": model.MustJSON(old)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	authored := func() json.RawMessage {
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return cf.Keys["authored"]
	}
	if n := authoredLen(t, authored()); n != 200 {
		t.Fatalf("setup: authored holds %d", n)
	}
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author200")}.Handle}}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v\n%s", err, out.String())
	}
	first := authored()
	if n := authoredLen(t, first); n != model.DefaultAuthorRecent {
		t.Fatalf("after the first round authored holds %d; want %d", n, model.DefaultAuthorRecent)
	}
	c, _ := st.LoadCritic(ctx, w.Name)
	if len(c.Runtime.Author.Seen) != model.DefaultAuthorRecent || c.Runtime.LastError != "" {
		t.Fatalf("seen %d, error %q", len(c.Runtime.Author.Seen), c.Runtime.LastError)
	}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatal(err)
	}
	if string(authored()) != string(first) {
		t.Fatal("an unchanged window rewrote authored")
	}
}
