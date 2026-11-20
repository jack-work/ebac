package bandstate

import (
	"context"
	"github.com/gluck/ebac/sdk/bandhub"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func mirror() model.Critic {
	return model.Critic{Name: "band-state", FormID: "@x", Source: model.SourceBandHub, Band: &model.BandRef{Socket: bandhub.DefaultSocket, Utility: []string{"leob", "ross"}}}
}

// Through the wire (InProc encodes like an exec): the first round sets the keys and stores the state; the state comes
// back on the next request and a quiet hub sets nothing. No events, no seat, Done every round.
func TestPluginRoundTripsItsState(t *testing.T) {
	ctx := context.Background()
	h := fixture(t)
	now := time.UnixMilli(h.Stats.Hub.Now)
	noTip := func(context.Context, string, []string) (map[string]string, error) { return nil, nil }
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
