package poll

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/livetest"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// A child write carrying Expect lands only while each expected key still holds the value the round read;
// else nothing of it is written (moved). Controls: a held expectation and no expectation write the changed keys.
// RED if the expect check goes (the moved rows patch) or compares bytes instead of JSON (the reordered row moves).
// An expected-absent key the seat created meanwhile also moves the form.
func TestChildPatchLandsOnlyWhileExpectHolds(t *testing.T) {
	cur := map[string]json.RawMessage{"gh": raw(`{"labels":["a"],"draft":false}`), "sync": raw(`{"at":"t0"}`), "old": raw(`1`)}
	set := map[string]json.RawMessage{"gh": raw(`{"labels":["a","b"],"draft":false}`), "sync": raw(`{"at":"t0"}`)}
	for _, tc := range []struct {
		name   string
		expect map[string]json.RawMessage
		absent []string
		out    map[string]json.RawMessage
		gone   []string
		moved  bool
	}{
		{"no expect", nil, nil, map[string]json.RawMessage{"gh": set["gh"]}, []string{"old"}, false},
		{"expect holds", map[string]json.RawMessage{"gh": cur["gh"], "sync": cur["sync"]}, nil, map[string]json.RawMessage{"gh": set["gh"]}, []string{"old"}, false},
		{"expect holds, keys reordered", map[string]json.RawMessage{"gh": raw(`{"draft":false, "labels":["a"]}`)}, nil, map[string]json.RawMessage{"gh": set["gh"]}, []string{"old"}, false},
		{"gh moved (a seat edit)", map[string]json.RawMessage{"gh": raw(`{"labels":[],"draft":false}`)}, nil, nil, nil, true},
		{"sync moved (an ack)", map[string]json.RawMessage{"gh": cur["gh"], "sync": raw(`{}`)}, nil, nil, nil, true},
		{"expected absent, still absent", nil, []string{"parsed"}, map[string]json.RawMessage{"gh": set["gh"]}, []string{"old"}, false},
		{"expected absent, created meanwhile", nil, []string{"gh"}, nil, nil, true},
		{"expected key gone from the form", map[string]json.RawMessage{"parsed": raw(`{}`)}, nil, nil, nil, true},
	} {
		out, gone, moved := childPatch(cur, set, []string{"old", "absent"}, tc.expect, tc.absent)
		if !reflect.DeepEqual(out, tc.out) || !reflect.DeepEqual(gone, tc.gone) || moved != tc.moved {
			t.Errorf("%s: out %s gone %v moved %v; want %s %v %v", tc.name, model.MustJSON(out), gone, moved, model.MustJSON(tc.out), tc.gone, tc.moved)
		}
	}
}

// An expect key names one top-level key like a set key. RED if the check goes.
func TestValidateFormsExpectIsTopLevel(t *testing.T) {
	gc := &model.Critic{Name: "gh-cache", Source: model.SourceGHCache}
	ok := plugin.FormWrite{Key: "aether-pr-1", Set: map[string]json.RawMessage{"gh": raw(`{}`)}, Expect: map[string]json.RawMessage{"gh": raw(`{}`)}}
	if err := ValidateForms(gc, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err != nil {
		t.Fatalf("valid write refused: %v", err)
	}
	bad := ok
	bad.Expect = map[string]json.RawMessage{"gh.labels": raw(`[]`)}
	if err := ValidateForms(gc, &plugin.Reply{Forms: []plugin.FormWrite{bad}}); err == nil || err.Error() != `form write 0 may not expect "gh.labels" (top-level keys only): reply refused` {
		t.Fatalf("dotted expect: %v", err)
	}
}

// The GitHub cache pings only its trusted maintainer (model.GHWrite, from ebac's trusted config). Control:
// a ping to the maintainer passes. RED if github-cache loses its ping grant (control refused) or the maintainer
// check goes (the other rows pass).
func TestValidatePingsGHCacheOnlyTheTrustedMaintainer(t *testing.T) {
	w := &model.Critic{Name: "gh-cache", Source: model.SourceGHCache, Maintainer: "@0000000a"}
	ping := func(to string) *plugin.Reply {
		return &plugin.Reply{Pings: []plugin.Ping{{To: to, Text: "ghcache alarm conflict.draft on aether-pr-1"}}}
	}
	if err := ValidatePings(w, ping("@0000000a")); err != nil {
		t.Fatalf("the maintainer refused: %v", err)
	}
	const refused = "ping 0: the GitHub cache pings only its trusted maintainer: reply refused"
	noMaint := &model.Critic{Name: "gh-cache", Source: model.SourceGHCache, GHWrite: &model.GHWrite{}}
	for _, tc := range []struct {
		name string
		w    *model.Critic
		to   string
	}{
		{"another role", w, "@0000000b"},
		{"no trusted maintainer", noMaint, "@0000000a"},
	} {
		if err := ValidatePings(tc.w, ping(tc.to)); err == nil || err.Error() != refused {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if err := ValidatePings(w, &plugin.Reply{Pings: []plugin.Ping{{To: "@0000000a", Text: strings.Repeat("x", 401)}}}); err == nil {
		t.Errorf("a 401-byte ping to the maintainer passed")
	}
}

// Live, through the core: a github-cache round's request carries each child's gh and sync as the form
// holds them now; a child write whose Expect no longer holds (a seat edited gh while the plugin ran) does not land,
// is not an error, and leaves the seat's edit; with the Expect the form now holds, the same write lands. RED if
// readOwned stops reading gh/sync for the cache, or the expect check goes (the stale write lands over the edit).
func TestLiveGHCacheReadsTheFormAndLandsOnlyOnExpect(t *testing.T) {
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
	critForm, err := f.FormNew(ctx, "ebac-livetest-ghc")
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, f, critForm)
	name := "livetest-ghc-" + strings.TrimPrefix(critForm, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceGHCache, FormID: critForm, EverySec: 120, CreatedAt: time.Now().UTC()}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	var reply plugin.Reply
	var seen plugin.Request
	var during func()
	handle := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		seen = req
		if during != nil {
			during()
			during = nil
		}
		r := reply
		c := req.Critic
		r.Critic, r.Done = &c, true
		return &r, nil
	}
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": handle}}
	load := func() *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	gh := func(id string) string {
		t.Helper()
		k, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(k["gh"])
	}
	const key = "widget-pr-7"
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Set: map[string]json.RawMessage{"gh": raw(`{"labels":[]}`), "sync": raw(`{"at":"t1"}`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 1: %v\n%s", err, out.String())
	}
	child := load().Runtime.Children[key]
	if child == "" {
		t.Fatalf("no child\n%s", out.String())
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, child) })

	// A seat edits gh: the next request carries it.
	if err := f.SetJSON(ctx, child, "gh", map[string]any{"labels": []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	reply = plugin.Reply{}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if !jsonEqual(seen.Owned[key]["gh"], raw(`{"labels":["a"]}`)) || !jsonEqual(seen.Owned[key]["sync"], raw(`{"at":"t1"}`)) {
		t.Fatalf("owned %v", seen.Owned)
	}

	// The seat edits again while the plugin runs: the write expecting what the core read does not land.
	during = func() { _ = f.SetJSON(ctx, child, "gh", map[string]any{"labels": []string{"b"}}) }
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Set: map[string]json.RawMessage{"gh": raw(`{"labels":["c"]}`)},
		Expect: map[string]json.RawMessage{"gh": raw(`{"labels":["a"]}`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if n := load().Runtime.FormsLanded; n != 0 || !jsonEqual(raw(gh(child)), raw(`{"labels":["b"]}`)) || !strings.Contains(out.String(), "not landed") {
		t.Fatalf("stale write: landed %d, gh %s, out %q", n, gh(child), out.String())
	}
	// Control: expecting what the form holds now, the same write lands.
	reply.Forms[0].Expect = map[string]json.RawMessage{"gh": raw(`{"labels":["b"]}`)}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 4: %v", err)
	}
	if n := load().Runtime.FormsLanded; n != 1 || !jsonEqual(raw(gh(child)), raw(`{"labels":["c"]}`)) {
		t.Fatalf("control: landed %d, gh %s", n, gh(child))
	}
}
