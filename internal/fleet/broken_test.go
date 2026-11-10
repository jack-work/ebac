package fleet

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

// Live: a form whose critic key is unusable ({}) is never a silent drop. Polling the critic by name fails naming the
// form, writes critic_error once (the holder's wake is gated on the same change), and ebac ls (the roster) shows an
// ERROR row for it. Control: before the wipe the critic polls and lists normally.
func TestLiveBrokenCriticIsAnError(t *testing.T) {
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
	name := "livetest-broken-" + time.Now().UTC().Format("150405")
	id, err := f.FormNew(ctx, "ebac-"+name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: name, Mode: model.ModeTeams, Source: "teams-chat", FormID: id, EverySec: 60, CreatedAt: time.Now().UTC(),
		Chat: &model.ChatRef{ID: "19:x@thread.v2"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	p := &poll.Poller{Store: st, Fig: f, Out: &strings.Builder{}, Plugins: plugin.InProc{}}
	if _, err := st.LoadCritic(ctx, name); err != nil {
		t.Fatalf("control: %v", err)
	}
	// The wipe, as it landed on 2026-11-10 (written past ebac's guards, the way the old form library did).
	if err := f.SetJSON(ctx, id, model.CriticKey, map[string]any{}); err == nil {
		t.Fatal("SetJSON wrote an empty critic key")
	}
	c, err := f.Node(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := c.Form(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(ctx, cur.Snapshot.SetPath(model.CriticKey, json.RawMessage(`{}`)).Diff(cur.Snapshot), cur.Version); err != nil {
		t.Fatal(err)
	}

	st2, _ := form.NewStore(f)
	st2.Root = st.Root
	p.Store = st2
	err = p.Poll(ctx, name)
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "unusable critic key") {
		t.Fatalf("poll of a wiped critic: %v", err)
	}
	read := func() (string, uint64) {
		keys, v, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var s string
		_ = json.Unmarshal(keys[model.CriticErrorKey], &s)
		return s, v
	}
	msg, v1 := read()
	if !strings.Contains(msg, "unusable critic key") {
		t.Fatalf("critic_error: %q", msg)
	}
	_ = p.Poll(ctx, name)
	if msg2, v2 := read(); msg2 != msg || v2 != v1 {
		t.Fatalf("a second poll rewrote critic_error (v%d -> v%d): the holder would be woken twice", v1, v2)
	}
	rows, sum, err := BuildRoster(ctx, st2, f, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Form == id && r.Critic == name && strings.HasPrefix(r.Health, "ERROR critic key unusable") {
			found = true
		}
	}
	if !found || sum.Unhealthy == 0 {
		t.Fatalf("no ERROR row for %s in ebac ls (unhealthy %d)", id, sum.Unhealthy)
	}
}
