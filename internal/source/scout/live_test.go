package scout

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

// Live, through the core on real (scratch) forms: the first pass writes one pr:<n> / issue:<n> entry per relevant
// item on the scout's own role form; a quiet pass costs one GitHub call and writes no entry; last_success
// advances on a good pass and not on one whose source read failed; a merge sends the record to the inputs form's
// ledger outbox.
func TestLiveScoutThroughTheCore(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	mint := func(name string, keys map[string]any) string {
		id, err := f.FormNew(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
		for k, v := range keys {
			if err := f.SetJSON(ctx, id, k, v); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	board := mint("livetest-board", map[string]any{"prs": map[string]any{}})
	inputs := mint("livetest-scout-inputs", map[string]any{"rule": policyText(t)})
	role := mint("ebac-livetest-scout", nil)
	wd := newWorld(t)
	name := "livetest-scout-" + strings.TrimPrefix(role, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceScout, Host: "microsoft.ghe.com", FormID: role, CreatedAt: time.Now().UTC(),
		EverySec: 120, BoardForm: board, InputsForm: inputs, Scout: &wd.watch}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	now := wd.now
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out,
		Plugins: plugin.InProc{"gh": Plugin{GH: wd.gh, Hub: wd.hub, Now: func() time.Time { return now }}.Handle}}
	load := func() *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("pass 1: %v\n%s", err, out.String())
	}
	keys, _, err := f.FormRaw(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pr:101", "pr:102", "pr:201", "issue:301"} {
		if len(keys[k]) == 0 {
			t.Fatalf("no %s on the role form\n%s", k, out.String())
		}
	}
	if keys["pr:103"] != nil || keys["issue:302"] != nil || !strings.Contains(string(keys["pr:101"]), `"merge_state":"`) {
		t.Fatalf("entries: pr:103 %s issue:302 %s pr:101 %s", keys["pr:103"], keys["issue:302"], keys["pr:101"])
	}
	if !load().Runtime.LastSuccess.Equal(now) {
		t.Fatalf("last_success %v after a good pass", load().Runtime.LastSuccess)
	}

	now = now.Add(2 * time.Minute)
	wd.gh.calls = 0
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("pass 2: %v\n%s", err, out.String())
	}
	if wd.gh.calls != 1 || !strings.Contains(out.String(), "1 GitHub call(s), 0 entr(y/ies) written") {
		t.Fatalf("quiet pass: %d GitHub calls\n%s", wd.gh.calls, out.String())
	}
	if c := load(); !c.Runtime.LastSuccess.Equal(now) {
		t.Fatalf("last_success did not advance on a quiet pass: %v", c.Runtime.LastSuccess)
	}

	good := now
	now = now.Add(2 * time.Minute)
	wd.gh.fail = errors.New("HTTP 502")
	if err := p.Poll(ctx, name); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a failed read passed: %v", err)
	}
	if c := load(); !c.Runtime.LastSuccess.Equal(good) || c.Runtime.ConsecutiveFailures != 1 {
		t.Fatalf("failed read: last_success %v (want %v), failures %d", c.Runtime.LastSuccess, good, c.Runtime.ConsecutiveFailures)
	}
	wd.gh.fail = nil

	it := wd.gh.items[101]
	it.state, it.mergedAt, it.updated = "MERGED", now.Format(time.RFC3339), now
	now = now.Add(2 * time.Minute)
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("merge pass: %v\n%s", err, out.String())
	}
	ik, _, _ := f.FormRaw(ctx, inputs)
	if !strings.Contains(string(ik["ledger_outbox"]), `"kind":"pr-final"`) {
		t.Fatalf("no ledger record: %s", ik["ledger_outbox"])
	}
	if c := load(); !c.Runtime.LastSuccess.Equal(now) || c.Runtime.ConsecutiveFailures != 0 {
		t.Fatalf("recovered: %v %d", c.Runtime.LastSuccess, c.Runtime.ConsecutiveFailures)
	}
}
