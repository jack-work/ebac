package rosterstate

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

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden-*.json from the current code")

// goldenKeys is what the roster plugin writes to its form for one request:
// the keys it sets and the keys it removes. Runtime timestamps are left out,
// so the file depends on the fixture alone, not on the clock.
type goldenKeys struct {
	Set   map[string]json.RawMessage `json:"set"`
	Unset []string                   `json:"unset"`
	Done  bool                       `json:"done"`
}

// TestWireRepliesMatchTheGolden pins the forms the plugin writes, through the
// JSON wire (InProc encodes both ways), for two rounds: a first round on an
// empty form, and a second round after the family changed. The golden was
// written at ebac 79bd5c4, before the plugin left core; a plugin that moved
// must write the same keys.
func TestWireRepliesMatchTheGolden(t *testing.T) {
	run := plugin.InProc{"roster": Plugin{Now: func() time.Time { return now }}.Handle}
	critic := model.Critic{Name: "roles", Source: model.SourceRoster}

	round := func(c model.Critic, arias []model.Aria) (*plugin.Reply, model.Critic) {
		t.Helper()
		rep, err := run.Call(context.Background(), "roster", plugin.Request{Protocol: plugin.Version, Op: plugin.OpRound, Critic: c, Arias: arias})
		if err != nil {
			t.Fatalf("round: %v", err)
		}
		return rep, *rep.Critic
	}

	first, after := round(critic, fixture())
	changed := fixture()
	changed = append(changed[:3], changed[4:]...) // the family root is reaped
	var kept []model.Aria
	for _, a := range changed {
		if a.ID != "@aaaa0004" { // the ghost role's form is deleted: its key goes
			kept = append(kept, a)
		}
	}
	changed = append(kept, model.Aria{ID: "@aaaa0009", Kind: "form", Name: "alpha-scout", TargetAria: "33333333"})
	second, _ := round(after, changed)

	for name, rep := range map[string]*plugin.Reply{"first": first, "second": second} {
		got := goldenKeys{Set: rep.Set, Unset: rep.Unset, Done: rep.Done}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "golden-"+name+".json")
		if *updateGolden {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("golden %s: %v (run with -update-golden at the base commit)", path, err)
		}
		if string(want) != string(b)+"\n" {
			t.Errorf("%s round wrote different keys than the golden %s:\n got: %s\nwant: %s", name, path, b, want)
		}
	}
}
