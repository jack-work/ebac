package poll

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
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// The scout may write one other form, its inputs form, and there only ledger_outbox; any other source may write none.
func TestValidateForms(t *testing.T) {
	scout := &model.Critic{Name: "s", Source: model.SourceScout, InputsForm: "@inputs"}
	ok := plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"108445":{}}`)}}
	if err := ValidateForms(scout, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err != nil {
		t.Fatalf("valid write refused: %v", err)
	}
	for name, c := range map[string]struct {
		w  *model.Critic
		fw plugin.FormWrite
		in string
	}{
		"another source":    {&model.Critic{Name: "x", Source: "teams-chat", InputsForm: "@inputs"}, ok, "may not write other forms"},
		"no target":         {scout, plugin.FormWrite{Set: ok.Set}, "names no target"},
		"no inputs form":    {&model.Critic{Name: "s", Source: model.SourceScout}, ok, "no inputs form"},
		"ledger_ack":        {scout, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_ack": raw(`{}`)}}, `inputs key "ledger_ack"`},
		"rule":              {scout, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"rule": raw(`""`)}}, `inputs key "rule"`},
		"dotted outbox key": {scout, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox.1": raw(`{}`)}}, `"ledger_outbox.1"`},
		"target-aria":       {scout, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"target-aria": raw(`"x"`)}}, `"target-aria"`},
	} {
		err := ValidateForms(c.w, &plugin.Reply{Forms: []plugin.FormWrite{ok, c.fw}})
		if err == nil || !strings.Contains(err.Error(), c.in) || !strings.Contains(err.Error(), "reply refused") {
			t.Errorf("%s: %v (want %q)", name, err, c.in)
		}
	}
}

// The scout's own keys are pr:<n> and issue:<n>, one segment, nothing else.
func TestScoutSourceKeys(t *testing.T) {
	ok := &plugin.Reply{Set: map[string]json.RawMessage{"pr:108445": raw(`{}`), "issue:96000": raw(`{}`)}, Unset: []string{"pr:1"}}
	if err := SourceKeysOnly(model.SourceScout, ok); err != nil {
		t.Fatalf("scout keys refused: %v", err)
	}
	for _, k := range []string{"pr:", "pr:1.title", "prs", "authored", "critic", "issue:*", "target-aria"} {
		if err := SourceKeysOnly(model.SourceScout, &plugin.Reply{Set: map[string]json.RawMessage{k: raw(`1`)}}); err == nil {
			t.Errorf("scout may set %q", k)
		}
	}
	if err := SourceKeysOnly("github-author", &plugin.Reply{Set: map[string]json.RawMessage{"pr:1": raw(`1`)}}); err == nil {
		t.Error("another source may set pr:1")
	}
}

// Live, through the core on scratch forms: the scout's entries land on its own role form, its outbox members merge
// into the inputs form's ledger_outbox (a null removes one) without touching the steward's keys, the board is read
// (prs, items) and never written, and a reply naming another inputs key is refused whole (nothing lands).
func TestLiveScoutFormsThroughTheCore(t *testing.T) {
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
	mint := func(name string) string {
		id, err := f.FormNew(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
		return id
	}
	critForm, boardForm, inputsForm := mint("ebac-livetest-scout"), mint("livetest-board"), mint("livetest-inputs")
	if err := f.SetJSON(ctx, boardForm, "items", map[string]string{"a": "hand-kept"}); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, inputsForm, "rule", "## band-jobs: band jobs"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, inputsForm, "ledger_outbox", map[string]any{"1": map[string]string{"pr": "1"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, inputsForm, "ledger_ack", map[string]any{"1": map[string]string{"sha": "k"}}); err != nil {
		t.Fatal(err)
	}
	name := "livetest-scout-" + strings.TrimPrefix(critForm, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceScout, FormID: critForm, BoardForm: boardForm, InputsForm: inputsForm,
		Scout: &model.ScoutWatch{Repo: "bic/aether", Authors: []string{"x"}}, EverySec: 120, CreatedAt: time.Now().UTC()}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	var reply plugin.Reply
	var seen plugin.Request
	handle := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		seen = req
		r := reply
		c := req.Critic
		r.Critic, r.Done = &c, true
		return &r, nil
	}
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": handle}}
	keys := func(id string) map[string]json.RawMessage {
		t.Helper()
		k, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	board0 := keys(boardForm)

	reply = plugin.Reply{Set: map[string]json.RawMessage{"pr:108445": raw(`{"n":108445,"state":"MERGED"}`), "issue:96000": raw(`{"n":96000}`)},
		Forms: []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"2":{"pr":"2"}}`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 1: %v\n%s", err, out.String())
	}
	if !strings.Contains(string(seen.Inputs["rule"]), "band-jobs") || !strings.Contains(string(seen.Board["items"]), "hand-kept") {
		t.Fatalf("the plugin did not get the inputs and the board: %v %v", seen.Inputs, seen.Board)
	}
	ck := keys(critForm)
	if !strings.Contains(string(ck["pr:108445"]), "MERGED") || string(ck["issue:96000"]) != `{"n":96000}` {
		t.Fatalf("entries on the role form: %v", ck)
	}
	ik := keys(inputsForm)
	if ob := string(ik["ledger_outbox"]); !strings.Contains(ob, `"1"`) || !strings.Contains(ob, `"2"`) || !strings.Contains(string(ik["ledger_ack"]), `"k"`) {
		t.Fatalf("outbox not merged or ack touched: %v", ik)
	}

	// Another inputs key: refused whole, the entry write in the same reply does not land.
	reply = plugin.Reply{Set: map[string]json.RawMessage{"pr:108445": raw(`{"n":108445,"state":"OPEN"}`)},
		Forms: []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"ledger_ack": raw(`{}`)}}}}
	if err := p.Poll(ctx, name); err == nil || !strings.Contains(err.Error(), "reply refused") {
		t.Fatalf("a ledger_ack write was not refused: %v", err)
	}
	if !strings.Contains(string(keys(critForm)["pr:108445"]), "MERGED") {
		t.Fatal("a refused reply partly landed")
	}

	// A null member removes it; an unset entry goes.
	reply = plugin.Reply{Unset: []string{"issue:96000"}, Forms: []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"1":null}`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 3: %v\n%s", err, out.String())
	}
	if ob := string(keys(inputsForm)["ledger_outbox"]); strings.Contains(ob, `"1"`) || !strings.Contains(ob, `"2"`) {
		t.Fatalf("outbox after null: %s", ob)
	}
	if _, still := keys(critForm)["issue:96000"]; still {
		t.Fatal("unset entry still on the form")
	}
	b1 := keys(boardForm)
	if string(b1["items"]) != string(board0["items"]) || len(b1) != len(board0) {
		t.Fatalf("the board moved: %v -> %v", board0, b1)
	}
}
