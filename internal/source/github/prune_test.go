package github

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

func prState(owner, repo string, n int, state string, updated time.Time) *model.PRState {
	return &model.PRState{Key: model.MakePRKey(owner, repo, n), Owner: owner, Repo: repo, Number: n, State: state,
		UpdatedAt: updated.UTC().Format(time.RFC3339), Threads: map[string]*model.Thread{"T": {ID: "T"}}}
}

func critic(prs ...*model.PRState) (*model.Critic, *model.Snapshot) {
	w := &model.Critic{Name: "c", Stop: model.DefaultStop()}
	snap := &model.Snapshot{PRs: map[model.PRKey]*model.PRState{}}
	for _, p := range prs {
		w.PRs = append(w.PRs, model.PRRef{Owner: p.Owner, Repo: p.Repo, Number: p.Number})
		snap.PRs[p.Key] = p
	}
	return w, snap
}

func TestPruneClosedPastGrace(t *testing.T) {
	now := time.Date(2026, 11, 9, 23, 0, 0, 0, time.UTC)
	w, snap := critic(
		prState("acme", "w", 1, "OPEN", now.Add(-48*time.Hour)),
		prState("acme", "w", 2, "MERGED", now.Add(-7*time.Hour)), // past the 6h default grace
		prState("acme", "w", 3, "MERGED", now.Add(-time.Hour)),   // inside it
		prState("acme", "w", 4, "CLOSED", now.Add(-30*24*time.Hour)),
	)
	got := pruneClosed(w, snap, now)
	if strings.Join(got, ",") != "acme/w#2,acme/w#4" {
		t.Fatalf("dropped %v; want #2 and #4", got)
	}
	if len(w.PRs) != 2 || snap.PRs["acme/w#2"] != nil || snap.PRs["acme/w#4"] != nil {
		t.Fatalf("after prune: prs %v snapshot %d", w.PRs, len(snap.PRs))
	}
	if snap.PRs["acme/w#3"] == nil || snap.PRs["acme/w#1"] == nil {
		t.Fatal("control: an open PR and a merge inside the grace must stay")
	}

	// A critic whose PRs are ALL closed is the archival path's: nothing is dropped.
	w, snap = critic(prState("acme", "w", 2, "MERGED", now.Add(-7*time.Hour)))
	if got := pruneClosed(w, snap, now); got != nil || len(w.PRs) != 1 {
		t.Fatalf("an all-closed critic was pruned: %v", got)
	}

	// The critic's own grace is the one used.
	w, snap = critic(prState("acme", "w", 1, "OPEN", now), prState("acme", "w", 2, "MERGED", now.Add(-7*time.Hour)))
	w.Stop.ArchiveGraceSec = 24 * 3600
	if got := pruneClosed(w, snap, now); got != nil {
		t.Fatalf("a 24h-grace critic dropped a 7h-old merge: %v", got)
	}
}

func TestPruneClosedKeepsTheRecentWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 23, 0, 0, 0, time.UTC)
	prs := []*model.PRState{prState("acme", "w", 1, "OPEN", now)}
	for i := 0; i < 60; i++ {
		prs = append(prs, prState("acme", "w", 100+i, "MERGED", now.Add(-time.Duration(i)*time.Minute)))
	}
	w, snap := critic(prs...)
	got := pruneClosed(w, snap, now)
	if len(got) != 10 || len(w.PRs) != 1+model.RecentWindow {
		t.Fatalf("dropped %d, kept %d; want the %d newest closed kept", len(got), len(w.PRs), model.RecentWindow)
	}
	if snap.PRs["acme/w#100"] == nil || snap.PRs["acme/w#159"] != nil {
		t.Fatal("the window is not the most recently updated")
	}
}

// Live, through the core: a form written before the prune (prs holds a long-merged PR with its thread) loses that
// PR from prs, threads, the snapshot and the critic on its first round; a merge inside the grace stays.
func TestLiveLongMergedPRLeavesTheForm(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-prune")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	gh := forgetest.FakeGH(t, "prs-closed")
	w := &model.Critic{Name: "livetest-prune-" + strings.TrimPrefix(id, "@")[:8], FormID: id, Mode: model.ModeReview,
		Host: "microsoft.ghe.com", CreatedAt: time.Now().UTC(), Stop: model.DefaultStop()}
	pre := &model.Snapshot{Critic: w.Name, TakenAt: time.Now().UTC(), PRs: map[model.PRKey]*model.PRState{}, Complete: true}
	for n := 1; n <= 3; n++ {
		w.PRs = append(w.PRs, model.PRRef{Owner: "acme", Repo: "widget", Number: n})
		p, err := gh.FetchPR(ctx, "acme", "widget", n)
		if err != nil {
			t.Fatal(err)
		}
		pre.PRs[p.Key] = p
	}
	if err := st.Save(ctx, w, pre, nil); err != nil {
		t.Fatal(err)
	}
	prsKey := func() string {
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(cf.Keys["prs"])
	}
	before := prsKey()
	for _, want := range []string{"acme/widget#2", "T2", "acme/widget#3"} {
		if !strings.Contains(before, form.PrFormKey(model.PRKey(want))) && !strings.Contains(before, want) {
			t.Fatalf("setup: prs lacks %s: %.300s", want, before)
		}
	}

	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Me: "jack", Plugins: plugin.InProc{"gh": Plugin{GH: gh}.Handle}}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v\n%s", err, out.String())
	}
	after := prsKey()
	if strings.Contains(after, "acme/widget#2") || strings.Contains(after, `"T2"`) {
		t.Fatalf("the long-merged PR (or its thread) is still in prs: %.500s", after)
	}
	if !strings.Contains(after, "acme/widget#3") || !strings.Contains(after, "acme/widget#1") {
		t.Fatalf("control: the open PR and the merge inside the grace must stay: %.500s", after)
	}
	snap, err := st.LoadSnapshot(ctx, w.Name)
	if err != nil || snap.PRs["acme/widget#2"] != nil || snap.PRs["acme/widget#3"] == nil {
		t.Fatalf("snapshot: %v, #2 %v", err, snap != nil && snap.PRs["acme/widget#2"] != nil)
	}
	c, _ := st.LoadCritic(ctx, w.Name)
	var keys []string
	for _, r := range c.PRs {
		keys = append(keys, string(r.Key()))
	}
	if strings.Join(keys, ",") != "acme/widget#1,acme/widget#3" {
		t.Fatalf("critic PRs %v", keys)
	}
	if !strings.Contains(out.String(), "dropped 1 closed/merged PR(s) past the archival grace: acme/widget#2") {
		t.Fatalf("the drop was not logged:\n%s", out.String())
	}
}
