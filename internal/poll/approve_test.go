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

var approveCritic = &model.Critic{Name: "ap", Source: model.SourceApprove, InputsForm: "@in",
	Requests: &model.ApproveWatch{PingTo: "@d407b690"}}

// approve-requests writes only results on its inputs form, pings only its
// configured role, and sets no key on its own role form.
func TestValidateApproveWrites(t *testing.T) {
	ok := plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"requests": raw(`{"r1":{"result":{"ok":false,"reason":"x","at":"t"}}}`)}}
	if err := ValidateForms(approveCritic, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err != nil {
		t.Fatalf("a result write refused: %v", err)
	}
	for name, fw := range map[string]plugin.FormWrite{
		"other key":      {Inputs: true, Set: map[string]json.RawMessage{"grants": raw(`{}`)}},
		"request field":  {Inputs: true, Set: map[string]json.RawMessage{"requests": raw(`{"r1":{"repo":"bic/x"}}`)}},
		"result + field": {Inputs: true, Set: map[string]json.RawMessage{"requests": raw(`{"r1":{"result":{},"pr":2}}`)}},
		"not object":     {Inputs: true, Set: map[string]json.RawMessage{"requests": raw(`[1]`)}},
		"unset":          {Inputs: true, Unset: []string{"requests"}},
		"child form":     {Key: "x", Set: map[string]json.RawMessage{"a": raw(`1`)}},
		"role form":      {Set: map[string]json.RawMessage{"requests": raw(`{}`)}},
	} {
		if err := ValidateForms(approveCritic, &plugin.Reply{Forms: []plugin.FormWrite{ok, fw}}); err == nil || !strings.Contains(err.Error(), "reply refused") {
			t.Errorf("%s: not refused: %v", name, err)
		}
	}
	noInputs := *approveCritic
	noInputs.InputsForm = ""
	if err := ValidateForms(&noInputs, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err == nil {
		t.Error("a write without an inputs form was not refused")
	}
	if err := ValidatePings(approveCritic, &plugin.Reply{Pings: []plugin.Ping{{To: "@d407b690", Text: "approved"}}}); err != nil {
		t.Fatalf("the configured ping refused: %v", err)
	}
	if err := ValidatePings(approveCritic, &plugin.Reply{Pings: []plugin.Ping{{To: "@aaaaaaaa", Text: "approved"}}}); err == nil {
		t.Error("a ping to another role was not refused")
	}
	if err := SourceKeysOnly(model.SourceApprove, &plugin.Reply{Set: map[string]json.RawMessage{"x": raw(`1`)}}); err == nil {
		t.Error("a role-form key write was not refused")
	}
}

// Through the core, on real forms: the plugin gets the requests, a result
// lands member-wise (the requester's fields kept), a decided request's result
// is never overwritten, and another inputs key is refused whole.
func TestLiveApproveResultsThroughTheCore(t *testing.T) {
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
	critForm, inputsForm := mint("ebac-livetest-approve"), mint("livetest-approve-requests")
	if err := f.SetJSON(ctx, inputsForm, "requests", map[string]any{
		"r1":   map[string]any{"repo": "bic/aether", "pr": 1, "teams_msg_id": "1794516486156", "chat": "ahmed", "by": "box"},
		"done": map[string]any{"repo": "bic/aether", "pr": 2, "result": map[string]any{"ok": false, "reason": "kept", "at": "t0"}},
	}); err != nil {
		t.Fatal(err)
	}
	name := "livetest-approve-" + strings.TrimPrefix(critForm, "@")[:8]
	w := &model.Critic{Name: name, Source: model.SourceApprove, FormID: critForm, InputsForm: inputsForm,
		Requests: &model.ApproveWatch{Chat: "19:x", ChatAlias: "ahmed", AskFrom: "8:orgid:x", Grant: "g", Principal: "p"},
		EverySec: 60, CreatedAt: time.Now().UTC()}
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
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"approve": handle}}
	reqs := func() map[string]map[string]json.RawMessage {
		t.Helper()
		k, _, err := f.FormRaw(ctx, inputsForm)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]map[string]json.RawMessage
		if err := json.Unmarshal(k["requests"], &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	reply = plugin.Reply{Forms: []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"requests": raw(
		`{"r1":{"result":{"ok":false,"reason":"grant: none","at":"t1"}},"done":{"result":{"ok":true,"at":"t1"}},"gone":{"result":{"ok":true}}}`)}}}}
	if err := p.Poll(ctx, name); err != nil {
		t.Fatalf("round 1: %v\n%s", err, out.String())
	}
	if !strings.Contains(string(seen.Inputs["requests"]), "1794516486156") {
		t.Fatalf("the plugin did not get the requests: %v", seen.Inputs)
	}
	m := reqs()
	if !strings.Contains(string(m["r1"]["result"]), "grant: none") || string(m["r1"]["by"]) != `"box"` || string(m["r1"]["pr"]) != "1" {
		t.Fatalf("r1 not merged member-wise: %v", m["r1"])
	}
	if !strings.Contains(string(m["done"]["result"]), "kept") {
		t.Fatalf("a decided result was overwritten: %v", m["done"])
	}
	if _, ok := m["gone"]; ok {
		t.Fatalf("a result for a request that does not exist created it: %v", m)
	}

	// Another inputs key: refused whole.
	reply = plugin.Reply{Forms: []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"rule": raw(`"x"`)}}}}
	if err := p.Poll(ctx, name); err == nil || !strings.Contains(err.Error(), "reply refused") {
		t.Fatalf("an inputs key other than requests was not refused: %v", err)
	}
}
