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

func TestValidateForms(t *testing.T) {
	board := &model.Critic{Name: "b", Source: model.SourceBoard, BoardForm: "@board", InputsForm: "@inputs"}
	ok := []plugin.FormWrite{
		{Key: "pr-108445", Set: map[string]json.RawMessage{"state": raw(`"OPEN"`), "checklist": raw(`[]`)}},
		{Key: "pr-96046", Retire: true},
		{Board: true, Set: map[string]json.RawMessage{"prs": raw(`{}`)}},
		{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"108445":{}}`)}},
	}
	if err := ValidateForms(board, &plugin.Reply{Forms: ok}); err != nil {
		t.Fatalf("valid writes refused: %v", err)
	}
	for name, c := range map[string]struct {
		w  *model.Critic
		fw plugin.FormWrite
		in string
	}{
		"another source":      {&model.Critic{Name: "x", Source: "teams-chat"}, plugin.FormWrite{Key: "pr-1"}, "may not write other forms"},
		"no target":           {board, plugin.FormWrite{Set: map[string]json.RawMessage{"a": raw(`1`)}}, "0 targets"},
		"two targets":         {board, plugin.FormWrite{Key: "pr-1", Board: true}, "2 targets"},
		"bad child key":       {board, plugin.FormWrite{Key: "PR 1"}, "is not [a-z0-9-]"},
		"note on a child":     {board, plugin.FormWrite{Key: "pr-1", Set: map[string]json.RawMessage{"note": raw(`{}`)}}, `"note" (owner or core key)`},
		"note unset":          {board, plugin.FormWrite{Key: "pr-1", Unset: []string{"note"}}, `unset "note"`},
		"note on the board":   {board, plugin.FormWrite{Board: true, Set: map[string]json.RawMessage{"note": raw(`{}`)}}, `"note" (owner or core key)`},
		"dotted note":         {board, plugin.FormWrite{Key: "pr-1", Set: map[string]json.RawMessage{"note.next": raw(`"x"`)}}, "top-level keys only"},
		"system":              {board, plugin.FormWrite{Key: "pr-1", Set: map[string]json.RawMessage{"system": raw(`{}`)}}, "owner or core key"},
		"target-aria":         {board, plugin.FormWrite{Board: true, Unset: []string{"target-aria"}}, "owner or core key"},
		"board key not ours":  {board, plugin.FormWrite{Board: true, Set: map[string]json.RawMessage{"items": raw(`{}`)}}, `board key "items"`},
		"inputs key not ours": {board, plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_ack": raw(`{}`)}}, `inputs key "ledger_ack"`},
		"board retire":        {board, plugin.FormWrite{Board: true, Retire: true}, "only a child form retires"},
		"no board form":       {&model.Critic{Name: "b", Source: model.SourceBoard}, plugin.FormWrite{Board: true}, "no board form"},
	} {
		err := ValidateForms(c.w, &plugin.Reply{Forms: append(append([]plugin.FormWrite{}, ok[0]), c.fw)})
		if err == nil || !strings.Contains(err.Error(), c.in) || !strings.Contains(err.Error(), "reply refused") {
			t.Errorf("%s: %v (want %q)", name, err, c.in)
		}
	}
}

// Live, through the core: a board-sync critic's plugin writes a child PR form (minted with a ChildLease lifetime and
// recorded in runtime.children), the board's prs key and a ledger outbox member, and reads the inputs form; a reply
// naming note is refused whole (nothing lands); a retire renames the child and drops it from runtime.children; the
// outbox merges members and removes a null one.
func TestLiveBoardFormsThroughTheCore(t *testing.T) {
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
	critForm, boardForm, inputsForm := mint("ebac-livetest-boardsync"), mint("livetest-board"), mint("livetest-inputs")
	if err := f.SetJSON(ctx, boardForm, "items", map[string]string{"a": "hand-kept"}); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, inputsForm, "rule", "## band-jobs: band jobs"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, inputsForm, "ledger_outbox", map[string]any{"1": map[string]string{"pr": "1"}}); err != nil {
		t.Fatal(err)
	}
	name := "livetest-boardsync-" + strings.TrimPrefix(critForm, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceBoard, FormID: critForm, BoardForm: boardForm, InputsForm: inputsForm,
		EverySec: 120, CreatedAt: time.Now().UTC()}
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

	reply = plugin.Reply{Forms: []plugin.FormWrite{
		{Key: "pr-108445", Set: map[string]json.RawMessage{"state": raw(`"OPEN"`), "head": raw(`"abc"`)}},
		{Board: true, Set: map[string]json.RawMessage{"prs": raw(`{"ledger-1":{"pr":108445,"front":true}}`)}},
		{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"2":{"pr":"2"}}`)}},
	}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 1: %v\n%s", err, out.String())
	}
	if !strings.Contains(string(seen.Inputs["rule"]), "band-jobs") {
		t.Fatalf("the plugin did not get the inputs form: %v", seen.Inputs)
	}
	child := load().Runtime.Children["pr-108445"]
	if child == "" {
		t.Fatalf("no child recorded: %+v", load().Runtime.Children)
	}
	ck := keys(child)
	if string(ck["state"]) != `"OPEN"` || string(ck["kind"]) != `"ebac.child"` || string(ck["name"]) != `"ebac-`+name+`-pr-108445"` || string(ck["parent_form"]) != `"`+critForm+`"` {
		t.Fatalf("child keys: %v", ck)
	}
	arias, err := f.ListArias(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease := time.Duration(-1)
	for _, a := range arias {
		if a.ID == child {
			lease = time.Duration(a.TTLMS) * time.Millisecond
		}
	}
	if lease != model.ChildLease {
		t.Fatalf("child lease %s, want %s", lease, model.ChildLease)
	}
	bk := keys(boardForm)
	if !strings.Contains(string(bk["prs"]), "108445") || !strings.Contains(string(bk["items"]), "hand-kept") {
		t.Fatalf("board: %v", bk)
	}
	if ob := string(keys(inputsForm)["ledger_outbox"]); !strings.Contains(ob, `"1"`) || !strings.Contains(ob, `"2"`) {
		t.Fatalf("outbox not merged: %s", ob)
	}
	if _, req := seen.Children["pr-108445"]; req {
		t.Fatal("round 1 request already named a child it had not minted")
	}

	// note is refused whole: neither the child's state nor the board moves.
	reply = plugin.Reply{Forms: []plugin.FormWrite{
		{Key: "pr-108445", Set: map[string]json.RawMessage{"state": raw(`"MERGED"`)}},
		{Key: "pr-108445", Set: map[string]json.RawMessage{"note": raw(`{"next":"x"}`)}},
	}}
	if err := p.Poll(ctx, name); err == nil || !strings.Contains(err.Error(), "reply refused") {
		t.Fatalf("a note write was not refused: %v", err)
	}
	if string(keys(child)["state"]) != `"OPEN"` {
		t.Fatal("a refused reply partly landed")
	}

	// Retire after a final write; outbox member 1 removed by null.
	reply = plugin.Reply{Forms: []plugin.FormWrite{
		{Key: "pr-108445", Set: map[string]json.RawMessage{"state": raw(`"MERGED"`)}, Retire: true},
		{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{"1":null}`)}},
	}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("retire round: %v\n%s", err, out.String())
	}
	if seen.Children["pr-108445"] != child {
		t.Fatalf("the request did not cite the child: %v", seen.Children)
	}
	ck = keys(child)
	if string(ck["state"]) != `"MERGED"` || !strings.HasPrefix(string(ck["name"]), `"`+model.ArchivedFormPrefix) {
		t.Fatalf("retired child: %v", ck)
	}
	if _, still := load().Runtime.Children["pr-108445"]; still {
		t.Fatal("a retired child is still renewed")
	}
	if ob := string(keys(inputsForm)["ledger_outbox"]); strings.Contains(ob, `"1"`) || !strings.Contains(ob, `"2"`) {
		t.Fatalf("outbox after null: %s", ob)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, child) })
}
