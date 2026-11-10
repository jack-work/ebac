package prboard

import (
	"context"
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

// Live, through the core on real (scratch) forms: the first pass mints one child form per open PR carrying the
// facts and the checklist, writes the board's prs; a quiet second pass costs one GitHub call and moves no child
// form.
func TestLiveBoardSyncThroughTheCore(t *testing.T) {
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
	board := mint("livetest-board", map[string]any{"prs": map[string]any{}, "stacks": map[string]any{"cache": map[string]any{"name": "CACHE"}}})
	inputs := mint("livetest-board-inputs", map[string]any{"rule": policyText(t)})
	role := mint("ebac-livetest-boardsync", nil)
	name := "livetest-boardsync-" + strings.TrimPrefix(role, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceBoard, Host: "microsoft.ghe.com", FormID: role, CreatedAt: time.Now().UTC(),
		EverySec: 120, BoardForm: board, InputsForm: inputs, BoardWatch: &model.BoardWatch{Author: "John-Kelliher", Repo: "bic/aether"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	wd := newWorld(t)
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": Plugin{GH: wd.gh, Hub: wd.hub}.Handle}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("pass 1: %v\n%s", err, out.String())
	}
	c, err := st.LoadCritic(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	kids := c.Runtime.Children
	t.Cleanup(func() {
		for _, id := range kids {
			_ = f.FormRemove(ctx, id)
		}
	})
	if len(kids) != 2 || kids["pr-101"] == "" {
		t.Fatalf("children %v\n%s", kids, out.String())
	}
	keys, _, err := f.FormRaw(ctx, kids["pr-101"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(keys["title"]), "[CACHE][1-2] first") || !strings.Contains(string(keys["checklist"]), `"id":"band-jobs"`) {
		t.Fatalf("pr-101 form: title %s checklist %.200s", keys["title"], keys["checklist"])
	}
	bk, _, _ := f.FormRaw(ctx, board)
	if !strings.Contains(string(bk["prs"]), `"pr":101`) {
		t.Fatalf("board prs %s\n%s", bk["prs"], out.String())
	}
	wd.gh.calls = 0
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("pass 2: %v\n%s", err, out.String())
	}
	if wd.gh.calls != 1 || !strings.Contains(out.String(), "1 GitHub call(s), 0 PR form(s) written") {
		t.Fatalf("quiet pass: %d GitHub calls\n%s", wd.gh.calls, out.String())
	}
}
