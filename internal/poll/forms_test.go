package poll

import (
	"context"
	"encoding/json"
	"fmt"
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
		"no target":         {scout, plugin.FormWrite{Set: ok.Set}, "writes only its inputs form"},
		"a child":           {scout, plugin.FormWrite{Key: "a--b", Set: ok.Set}, "writes only its inputs form"},
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

// Keel branches write only child forms: never the holder's code_ready and ready, keel CI's pr_url and pr_number,
// the form's own keys, or a dotted key; at most model.MaxChildren of them.
func TestValidateFormsKeelBranches(t *testing.T) {
	kb := &model.Critic{Name: "aether-work", Source: model.SourceKeelBranches}
	ok := plugin.FormWrite{Key: "john-kelliher--effort-x", Set: map[string]json.RawMessage{"head": raw(`"abc"`), "branch": raw(`"work/John-Kelliher/effort-x"`)}}
	if err := ValidateForms(kb, &plugin.Reply{Forms: []plugin.FormWrite{ok, {Key: "a--b", Retire: true}}}); err != nil {
		t.Fatalf("valid writes refused: %v", err)
	}
	for name, c := range map[string]struct {
		fw plugin.FormWrite
		in string
	}{
		"code_ready":  {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"code_ready": raw(`true`)}}, `"code_ready" on child a--b (owner or core key)`},
		"ready unset": {plugin.FormWrite{Key: "a--b", Unset: []string{"ready"}}, `unset "ready"`},
		"pr_url":      {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"pr_url": raw(`"x"`)}}, `"pr_url"`},
		"pr_number":   {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"pr_number": raw(`1`)}}, `"pr_number"`},
		"target-aria": {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"target-aria": raw(`"x"`)}}, "owner or core key"},
		"system":      {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"system": raw(`{}`)}}, "owner or core key"},
		"name":        {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"name": raw(`"x"`)}}, "owner or core key"},
		"dotted":      {plugin.FormWrite{Key: "a--b", Set: map[string]json.RawMessage{"code_ready.x": raw(`1`)}}, "top-level keys only"},
		"bad key":     {plugin.FormWrite{Key: "Work/x", Set: ok.Set}, "is not [a-z0-9-]"},
		"inputs":      {plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"ledger_outbox": raw(`{}`)}}, "only child forms"},
		"another src": {ok, ""},
	} {
		w := kb
		if name == "another src" {
			w = &model.Critic{Name: "x", Source: "keel-threads"}
			c.in = "may not write other forms"
		}
		err := ValidateForms(w, &plugin.Reply{Forms: []plugin.FormWrite{ok, c.fw}})
		if err == nil || !strings.Contains(err.Error(), c.in) || !strings.Contains(err.Error(), "reply refused") {
			t.Errorf("%s: %v (want %q)", name, err, c.in)
		}
	}
	// The cap: 100 live children may be rewritten; the 101st is refused, unless one retires in the same reply.
	full := &model.Critic{Name: "aether-work", Source: model.SourceKeelBranches, Runtime: model.Runtime{Children: map[string]string{}}}
	for i := 0; i < model.MaxChildren; i++ {
		full.Runtime.Children[fmt.Sprintf("o--t%d", i)] = "@x"
	}
	if err := ValidateForms(full, &plugin.Reply{Forms: []plugin.FormWrite{{Key: "o--t5", Set: ok.Set}}}); err != nil {
		t.Fatalf("a rewrite at the cap refused: %v", err)
	}
	if err := ValidateForms(full, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err == nil || !strings.Contains(err.Error(), "101 child forms") {
		t.Fatalf("the 101st child: %v", err)
	}
	if err := ValidateForms(full, &plugin.Reply{Forms: []plugin.FormWrite{{Key: "o--t5", Retire: true}, ok}}); err != nil {
		t.Fatalf("a retire freeing a slot: %v", err)
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

// Pings: only the scout pings, at most MaxPings a round, each one line of at most 400 bytes to a role @<hex>.
func TestValidatePings(t *testing.T) {
	scout := &model.Critic{Name: "s", Source: model.SourceScout}
	ok := plugin.Ping{To: "@8d70861a", Text: "scout: issue #1 changed: body by alice; linked PR #2; https://x/1"}
	if err := ValidatePings(scout, &plugin.Reply{Pings: []plugin.Ping{ok}}); err != nil {
		t.Fatalf("valid ping refused: %v", err)
	}
	many := make([]plugin.Ping, MaxPings+1)
	for i := range many {
		many[i] = ok
	}
	for name, c := range map[string]struct {
		w     *model.Critic
		pings []plugin.Ping
		in    string
	}{
		"another source": {&model.Critic{Name: "x", Source: "github-author"}, []plugin.Ping{ok}, "may not ping"},
		"too many":       {scout, many, "pings in one round"},
		"an aria id":     {scout, []plugin.Ping{{To: "8d70861a", Text: "x"}}, "not a role form"},
		"two lines":      {scout, []plugin.Ping{{To: ok.To, Text: "a\nb"}}, "one line"},
		"too long":       {scout, []plugin.Ping{{To: ok.To, Text: strings.Repeat("x", 401)}}, "one line"},
		"empty":          {scout, []plugin.Ping{{To: ok.To}}, "one line"},
	} {
		if err := ValidatePings(c.w, &plugin.Reply{Pings: c.pings}); err == nil || !strings.Contains(err.Error(), c.in) {
			t.Errorf("%s: %v (want %q)", name, err, c.in)
		}
	}
}

// A validated round's pings go to each role's holder; a role nobody holds is skipped and said.
func TestLandFormsSendsPings(t *testing.T) {
	var sent []string
	var out strings.Builder
	p := &Poller{Out: &out,
		Send:   func(_ context.Context, to, msg string) error { sent = append(sent, to+" "+msg); return nil },
		Holder: func(_ context.Context, to string) bool { return to != "@deadbeef" }}
	w := &model.Critic{Name: "s", Source: model.SourceScout}
	err := p.landForms(context.Background(), w, &plugin.Reply{Pings: []plugin.Ping{{To: "@8d70861a", Text: "one"}, {To: "@deadbeef", Text: "two"}}})
	if err != nil || len(sent) != 1 || sent[0] != "@8d70861a one" || !strings.Contains(out.String(), "ping to @deadbeef skipped (no holder)") {
		t.Fatalf("err %v sent %v out %q", err, sent, out.String())
	}
	sent = nil
	if err := p.landForms(context.Background(), &model.Critic{Name: "a", Source: "github-author"}, &plugin.Reply{Pings: []plugin.Ping{{To: "@8d70861a", Text: "x"}}}); err != nil || len(sent) != 0 {
		t.Fatalf("another source's pings were sent: %v", sent)
	}
}

// Live, through the core: a keel-branches plugin's child write mints the child (name ebac-<critic>-<key>, system.ttl
// ChildLease, kind ebac.child) and records it; the next round's request cites it and carries its holder-owned keys;
// a reply writing code_ready is refused whole; a removed child is minted again on its next write; a retire renames it
// ebac-archived-... and drops it from runtime.children; children are never listed as critics or broken critics.
func TestLiveChildFormsThroughTheCore(t *testing.T) {
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
	critForm, err := f.FormNew(ctx, "ebac-livetest-kb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, critForm) })
	name := "livetest-kb-" + strings.TrimPrefix(critForm, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceKeelBranches, FormID: critForm, EverySec: 120, CreatedAt: time.Now().UTC(),
		Branches: &model.KeelBranches{Repo: "aether/aether"}}
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
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"keel": handle}}
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
	const key = "john-kelliher--effort-x"
	var kids []string
	t.Cleanup(func() {
		for _, id := range kids {
			_ = f.FormRemove(ctx, id)
		}
	})

	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Set: map[string]json.RawMessage{"branch": raw(`"work/John-Kelliher/effort-x"`), "head": raw(`"abc"`)}}},
		Set: map[string]json.RawMessage{"branches": raw(`{}`)}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 1: %v\n%s", err, out.String())
	}
	child := load().Runtime.Children[key]
	if child == "" {
		t.Fatalf("no child recorded\n%s", out.String())
	}
	kids = append(kids, child)
	ck := keys(child)
	if string(ck["head"]) != `"abc"` || string(ck["kind"]) != `"ebac.child"` || string(ck["name"]) != `"ebac-`+name+`-`+key+`"` || string(ck["parent_form"]) != `"`+critForm+`"` {
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
	critics, err := st.ListCritics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	broken, _ := st.BrokenCritics(ctx)
	for _, c := range critics {
		if c.FormID == child {
			t.Fatal("a child form listed as a critic")
		}
	}
	for _, b := range broken {
		if b.FormID == child {
			t.Fatal("a child form listed as a broken critic")
		}
	}

	// The holder sets code_ready: the next request carries it, and the child.
	if err := f.SetJSON(ctx, child, "code_ready", true); err != nil {
		t.Fatal(err)
	}
	reply = plugin.Reply{}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if seen.Children[key] != child || string(seen.Owned[key]["code_ready"]) != "true" {
		t.Fatalf("request: children %v owned %v", seen.Children, seen.Owned)
	}

	// code_ready written by the plugin: refused whole, the head write with it does not land.
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Set: map[string]json.RawMessage{"head": raw(`"def"`)}},
		{Key: key, Set: map[string]json.RawMessage{"code_ready": raw(`false`)}}}}
	if err := p.Poll(ctx, name); err == nil || !strings.Contains(err.Error(), "reply refused") {
		t.Fatalf("a code_ready write was not refused: %v", err)
	}
	if ck := keys(child); string(ck["head"]) != `"abc"` || string(ck["code_ready"]) != "true" {
		t.Fatalf("a refused reply partly landed: %v", ck)
	}

	// Removed under us: the next write mints a new child.
	if err := f.FormRemove(ctx, child); err != nil {
		t.Fatal(err)
	}
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Set: map[string]json.RawMessage{"head": raw(`"ghi"`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("re-mint round: %v\n%s", err, out.String())
	}
	again := load().Runtime.Children[key]
	if again == "" || again == child || string(keys(again)["head"]) != `"ghi"` {
		t.Fatalf("not re-minted: %q", again)
	}
	kids = append(kids, again)

	// Retire: renamed, dropped from runtime.children (no longer renewed), never killed.
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Key: key, Retire: true}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("retire round: %v", err)
	}
	if ck := keys(again); !strings.HasPrefix(string(ck["name"]), `"`+model.ArchivedFormPrefix) || string(ck["head"]) != `"ghi"` {
		t.Fatalf("retired child: %v", ck)
	}
	if _, still := load().Runtime.Children[key]; still {
		t.Fatal("a retired child is still renewed")
	}
}
