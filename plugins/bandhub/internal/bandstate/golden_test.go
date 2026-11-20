package bandstate

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden-round.json from the current code")

// The plugin's whole reply for two rounds over the recorded hub (a first round
// from nothing, then a round one minute later carrying the first round's
// state) is pinned byte for byte. Moving the plugin out of core must not change
// one byte of what it asks the core to write.
func TestGoldenRoundsAreByteIdentical(t *testing.T) {
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
		return rep
	}
	w := mirror()
	r1 := call(w, now)
	core := w
	plugin.Adopt(&core, r1.Critic)
	h.Stats.Hub.Now += 60_000
	r2 := call(core, now.Add(time.Minute))
	got, err := json.MarshalIndent([]*plugin.Reply{r1, r2}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden-round.json")
	if *updateGolden {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-golden at the base commit to record it)", err)
	}
	if len(want) < 1000 {
		t.Fatalf("golden is %d bytes: an empty golden proves nothing", len(want))
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("reply differs from the golden recorded at the base commit:\n got %d bytes\nwant %d bytes", len(got)+1, len(want))
	}
}
