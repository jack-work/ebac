package author

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

// ghRow is one `gh pr list --json` row as the fake serves it.
type ghRow struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	IsDraft   bool   `json:"isDraft"`
	Head      string `json:"headRefOid"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	MergedAt  string `json:"mergedAt"`
	ClosedAt  string `json:"closedAt"`
}

// prList is a scripted upstream: two PRs, written to the file the fake gh serves.
type prList struct {
	t    *testing.T
	path string
	rows []ghRow
}

func newPRList(t *testing.T) *prList {
	l := &prList{t: t, path: filepath.Join(t.TempDir(), "prs.json")}
	t.Setenv("EBAC_FAKE_PRS", l.path)
	l.rows = []ghRow{
		{Number: 101, URL: "https://x/pull/101", Title: "feat: one", State: "OPEN", Head: "aaaaaaaaaaaaaaaa", CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-10T00:00:00Z"},
		{Number: 102, URL: "https://x/pull/102", Title: "feat: two", State: "OPEN", IsDraft: true, Head: "bbbbbbbbbbbbbbbb", CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-10T00:00:00Z"},
	}
	l.write()
	return l
}

func (l *prList) write() {
	b, _ := json.Marshal(l.rows)
	if err := os.WriteFile(l.path, b, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// push moves an open PR's head and updated_at: what every push does upstream.
func (l *prList) push(i, n int) {
	l.rows[i].Head = fmt.Sprintf("c0ffee%02d%02d000000000000", i, n) // differs within the 12 hex the runtime keeps
	l.rows[i].UpdatedAt = time.Date(2026, 11, 10, 1, n, 0, 0, time.UTC).Format(time.RFC3339)
	l.write()
}

// Pushes write nothing to the studied form; a merge writes that PR's line once and wakes (the PEZ merge path:
// event author_pr_merged, authored[k].state MERGED + merged, the key and field names as before); draft->ready
// writes once; the head and updated_at live in the runtime only.
func TestAuthoredKeyMovesOnlyOnTransitions(t *testing.T) {
	l := newPRList(t)
	pl := plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author-file")}.Handle}
	w := authorWatch()
	call := func() *plugin.Reply {
		t.Helper()
		rep, err := pl.Call(context.Background(), "gh", plugin.Request{Op: plugin.OpRound, Critic: w})
		if err != nil || rep.Error != "" {
			t.Fatalf("round: %v %s", err, rep.Error)
		}
		plugin.Adopt(&w, rep.Critic)
		return rep
	}
	r := call()
	if r.Set["authored"] == nil {
		t.Fatal("baseline wrote no authored key")
	}
	for _, field := range []string{`"head"`, `"updated"`, `"created"`} {
		if strings.Contains(string(r.Set["authored"]), field) {
			t.Fatalf("authored carries per-push field %s: %s", field, r.Set["authored"])
		}
	}

	writes := 0
	for n := 1; n <= 5; n++ {
		l.push(0, n)
		l.push(1, n)
		if r := call(); r.Set["authored"] != nil {
			writes++
		}
	}
	if writes != 0 {
		t.Fatalf("5 pushes to 2 open PRs wrote authored %d times; want 0", writes)
	}
	if w.Runtime.Author.Seen["bic/aether#101"].Head != "c0ffee000500" {
		t.Fatalf("the runtime lost the head: %+v", w.Runtime.Author.Seen["bic/aether#101"])
	}

	// Merge: one write, that PR's line, and the wake event the PEZ trigger reads.
	// Stamped relative to now: a merged PR stays on the key for ClosedKeep after its merge.
	mergedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	l.rows[0].State, l.rows[0].MergedAt, l.rows[0].ClosedAt = "MERGED", mergedAt, mergedAt
	l.write()
	r = call()
	var view map[string]map[string]any
	if err := json.Unmarshal(r.Set["authored"], &view); err != nil {
		t.Fatalf("merge did not write authored: %v (%s)", err, r.Set["authored"])
	}
	m := view["bic/aether#101"]
	if m["state"] != "MERGED" || m["merged"] != mergedAt || m["n"] != float64(101) || m["url"] != "https://x/pull/101" || m["title"] != "feat: one" {
		t.Fatalf("merged line: %v", m)
	}
	if _, ok := m["closed"]; ok {
		t.Fatalf("a merged PR carries closed: %v", m)
	}
	if got := authorKinds(plugin.Unwire(r.Events)); got["bic/aether#101 author_pr_merged"] != model.TierWake {
		t.Fatalf("merge event: %v", got)
	}
	if r := call(); r.Set["authored"] != nil {
		t.Fatalf("an unchanged round after the merge wrote authored: %s", r.Set["authored"])
	}

	// draft -> ready: one write; close: one write with the closed instant.
	l.rows[1].IsDraft = false
	l.write()
	if r := call(); r.Set["authored"] == nil || authorKinds(plugin.Unwire(r.Events))["bic/aether#102 author_pr_new"] != model.TierWake {
		t.Fatalf("draft -> ready: set %v events %v", r.Set, authorKinds(plugin.Unwire(r.Events)))
	}
	closedAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	l.rows[1].State, l.rows[1].ClosedAt = "CLOSED", closedAt
	l.write()
	r = call()
	if !strings.Contains(string(r.Set["authored"]), `"closed":"`+closedAt+`"`) {
		t.Fatalf("close: %s", r.Set["authored"])
	}
}

// Live: through the core, on a real role form, N pushes leave every studied key (all but system.*) untouched; the
// merge changes the authored key. Control for the instrument: the merge round does change what it measures.
func TestLiveAuthorPushesWriteNothingStudied(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-author-quiet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := authorWatch()
	w.Name, w.FormID, w.CreatedAt = "livetest-aquiet-"+strings.TrimPrefix(id, "@")[:8], id, time.Now().UTC()
	if err := st.SaveCritic(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedTrigger(ctx, &w); err != nil {
		t.Fatal(err)
	}
	l := newPRList(t)
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author-file")}.Handle}}
	studied := func() string {
		t.Helper()
		keys, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		delete(keys, "system")
		b, _ := json.Marshal(keys)
		return string(b)
	}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("baseline: %v\n%s", err, out.String())
	}
	before := studied()
	if !strings.Contains(before, "bic/aether#101") {
		t.Fatalf("baseline authored missing: %s", before)
	}
	for n := 1; n <= 3; n++ {
		l.push(0, n)
		if err := p.Poll(ctx, w.Name); err != nil {
			t.Fatalf("push round %d: %v\n%s", n, err, out.String())
		}
	}
	if after := studied(); after != before {
		t.Fatalf("pushes changed studied keys:\n before %s\n after  %s", before, after)
	}
	l.rows[0].State, l.rows[0].MergedAt = "MERGED", time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339) // within ClosedKeep
	l.write()
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("merge round: %v\n%s", err, out.String())
	}
	if after := studied(); after == before || !strings.Contains(after, `"MERGED"`) {
		t.Fatalf("control: the merge did not reach the studied form: %s", after)
	}
}
