package bandstate

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

func mirror() model.Critic {
	return model.Critic{Name: "band-state", FormID: "@x", Source: model.SourceBandHub, Band: &model.BandRef{Socket: DefaultSocket, Utility: []string{"leob", "ross"}}}
}

// Through the wire (InProc encodes like an exec): the first round sets the keys and stores the state; the state comes
// back on the next request and a quiet hub sets nothing. No events, no seat, Done every round.
func TestPluginRoundTripsItsState(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	noTip := func(context.Context, string, string) (string, error) { return "", nil }
	call := func(w model.Critic, at time.Time) *plugin.Reply {
		t.Helper()
		rep, err := plugin.InProc{"bandhub": Plugin{Hub: h, Tip: noTip, Now: func() time.Time { return at }}.Handle}.
			Call(ctx, "bandhub", plugin.Request{Op: plugin.OpRound, Critic: w})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Done || len(rep.Events) != 0 || rep.Error != "" {
			t.Fatalf("reply: done %v events %d error %q", rep.Done, len(rep.Events), rep.Error)
		}
		return rep
	}
	w := mirror()
	r1 := call(w, now)
	if len(r1.Set) != 5 || len(r1.Critic.Runtime.BandHub) == 0 || strings.Join(r1.Unset, ",") != "live" {
		t.Fatalf("first round: %d keys, unset %v, state %d bytes", len(r1.Set), r1.Unset, len(r1.Critic.Runtime.BandHub))
	}
	core := w
	plugin.Adopt(&core, r1.Critic)
	if string(core.Runtime.BandHub) != string(r1.Critic.Runtime.BandHub) {
		t.Fatal("the core does not adopt the plugin's state")
	}
	h.Stats.Hub.Now += 60_000
	if r2 := call(core, now.Add(time.Minute)); len(r2.Set) != 0 || len(r2.Unset) != 0 {
		t.Fatalf("a quiet hub wrote %v", r2.Log)
	}
	// Control: without the carried state the same hub is written in full again.
	if r3 := call(w, now.Add(time.Minute)); len(r3.Set) != 5 {
		t.Fatalf("control: a stateless round wrote %d keys", len(r3.Set))
	}
}

// An unreachable hub is a failed round with effects, never a plugin crash.
func TestPluginHubDown(t *testing.T) {
	w := mirror()
	w.Band.Socket = t.TempDir() + "/no-hub.sock"
	rep, err := plugin.InProc{"bandhub": Plugin{}.Handle}.Call(context.Background(), "bandhub", plugin.Request{Op: plugin.OpRound, Critic: w})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Error, "no-hub.sock") || rep.Critic.Runtime.ConsecutiveFailures != 1 || len(rep.Set) != 0 {
		t.Fatalf("hub down: error %q failures %d set %d", rep.Error, rep.Critic.Runtime.ConsecutiveFailures, len(rep.Set))
	}
}

// Live: the core polls the mirror through the plugin on a real role form. The keys land; a second poll changes nothing
// (same form version); the empty role is not an error, because a mirror has no seat.
func TestLiveMirrorThroughTheCore(t *testing.T) {
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
	id, err := f.FormNew(ctx, "ebac-livetest-bandstate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := mirror()
	w.Name, w.FormID, w.CreatedAt = "livetest-bandstate-"+strings.TrimPrefix(id, "@")[:8], id, time.Now().UTC()
	if err := st.SaveCritic(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedTrigger(ctx, &w); err != nil {
		t.Fatal(err)
	}
	// A form an earlier version wrote: its live key must go.
	if err := f.SetJSON(ctx, id, "live", []string{"4246 old line"}); err != nil {
		t.Fatal(err)
	}
	h := fixture(t)
	var out strings.Builder
	poll1 := func() {
		t.Helper()
		noTip := func(context.Context, string, string) (string, error) { return "", nil }
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"bandhub": Plugin{Hub: h, Tip: noTip}.Handle}}
		if err := p.Poll(ctx, w.Name); err != nil {
			t.Fatalf("poll: %v\n%s", err, out.String())
		}
	}
	version := func() uint64 {
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return cf.Version
	}
	poll1()
	cf, err := st.ReadForm(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"queues", "nodes", "node_load", "dupes", "updated"} {
		if _, ok := cf.Keys[k]; !ok {
			t.Fatalf("key %s missing; keys: %v\n%s", k, keysOf(cf.Keys), out.String())
		}
	}
	if _, ok := cf.Keys["live"]; ok {
		t.Fatalf("the dropped key live survived round 1: %s", cf.Keys["live"])
	}
	c, _ := st.LoadCritic(ctx, w.Name)
	if c.Runtime.LastError != "" || len(c.Runtime.BandHub) == 0 {
		t.Fatalf("after round 1: error %q, state %d bytes", c.Runtime.LastError, len(c.Runtime.BandHub))
	}
	v1 := version()
	h.Stats.Hub.Now += 60_000
	poll1()
	if v2 := version(); v2 != v1 {
		// The core saves the critic each round; a quiet round may only touch ebac's own system keys.
		cf2, _ := st.ReadForm(ctx, id)
		for _, k := range []string{"queues", "nodes", "node_load", "dupes", "updated"} {
			if string(cf2.Keys[k]) != string(cf.Keys[k]) {
				t.Fatalf("a quiet round rewrote %s", k)
			}
		}
		t.Logf("form version %d -> %d on a quiet round (ebac system keys only; mirror keys unchanged)", v1, v2)
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Fatalf("second round not reported unchanged:\n%s", out.String())
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
