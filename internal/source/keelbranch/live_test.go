package keelbranch

import (
	"context"
	"encoding/json"
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

// Live, through the core on scratch forms (keel faked): pass 1 mints one form per work branch with its facts and
// branch key; pass 2 writes the registry on the critic form; the holder's code_ready reaches the plugin and pings the
// Reviewer; a new comment wakes the branch form's holder (sends faked: no aria is woken); last_success advances; a
// branch gone from keel retires its form by archive rename.
func TestLiveKeelBranchesThroughTheCore(t *testing.T) {
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
	role, err := f.FormNew(ctx, "ebac-livetest-kb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, role) })
	name := "livetest-kb-" + strings.TrimPrefix(role, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceKeelBranches, FormID: role, EverySec: 120, CreatedAt: time.Now().UTC(),
		Branches: &model.KeelBranches{Repo: "aether/aether", Reviewer: reviewer}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	wd := newWorld(t)
	now := wd.now
	var sent []string
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out,
		Plugins: plugin.InProc{"keel": Plugin{Keel: wd.k, Now: func() time.Time { return now }}.Handle},
		Send:    func(_ context.Context, to, msg string) error { sent = append(sent, to+" "+msg); return nil },
		Holder:  func(context.Context, string) bool { return true }}
	load := func() *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	keys := func(id string) map[string]json.RawMessage {
		t.Helper()
		k, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	round := func(what string) {
		t.Helper()
		now = now.Add(2 * time.Minute)
		if err := p.Poll(ctx, name); err != nil {
			t.Fatalf("%s: %v\n%s", what, err, out.String())
		}
	}
	round("pass 1")
	kids := load().Runtime.Children
	t.Cleanup(func() {
		for _, id := range kids {
			_ = f.FormRemove(ctx, id)
		}
	})
	jkForm := kids[jk]
	if len(kids) != 2 || jkForm == "" {
		t.Fatalf("children %v\n%s", kids, out.String())
	}
	ck := keys(jkForm)
	if string(ck["branch"]) != `"work/John-Kelliher/effort-x"` || string(ck["name"]) != `"ebac-`+name+`-`+jk+`"` || len(ck["files"]) == 0 {
		t.Fatalf("branch form %v", ck)
	}
	round("pass 2")
	var reg map[string]string
	_ = json.Unmarshal(keys(role)["branches"], &reg)
	if reg["work/John-Kelliher/effort-x"] != jkForm || !load().Runtime.LastSuccess.Equal(now) {
		t.Fatalf("registry %v, last_success %v", reg, load().Runtime.LastSuccess)
	}

	// The builder says code_ready; a reviewer comments.
	if err := f.SetJSON(ctx, jkForm, "code_ready", true); err != nil {
		t.Fatal(err)
	}
	wd.k.thread("t1", wd.k.head("work/John-Kelliher/effort-x"), "", "a.go", 3, comment("c1", "rev-a", "why?", now.Add(time.Minute)))
	round("pass 3")
	got := strings.Join(sent, "\n")
	if !strings.Contains(got, reviewer+" keel-branches: work/John-Kelliher/effort-x code_ready at") ||
		!strings.Contains(got, jkForm+" keel-branches: work/John-Kelliher/effort-x: 1 new comment(s): a.go:3@") || len(sent) != 2 {
		t.Fatalf("sent %q", got)
	}
	if string(keys(jkForm)["code_ready"]) != "true" {
		t.Fatal("the plugin's write touched code_ready")
	}

	delete(wd.k.branches, "work/chiqun/router")
	round("pass 4")
	if ck := keys(kids[cr]); !strings.HasPrefix(string(ck["name"]), `"`+model.ArchivedFormPrefix) {
		t.Fatalf("gone branch not retired: %s", ck["name"])
	}
	if _, still := load().Runtime.Children[cr]; still {
		t.Fatal("a retired form is still renewed")
	}
}
