package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	"github.com/jack-work/figaro/api/form"
)

// 2026-10-20: a form whose system member was a leaf lost its whole system
// subtree to a nested write (jack-work/figaro#43). Writes must not nest.
func TestNextBoardSurvivesALeafSystem(t *testing.T) {
	var cur form.Snapshot
	if err := json.Unmarshal([]byte(`{"name":"f","system":null,"post":{"p1":{"pr":"a/b#1","body":"x"}}}`), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := nextBoard(cur, map[string]json.RawMessage{"system.ebac": json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("a dotted key was accepted")
	}
	curKeys := map[string]json.RawMessage{}
	b, _ := json.Marshal(cur)
	_ = json.Unmarshal(b, &curKeys)

	system := model.ObjectOf(curKeys["system"])
	system["ebac"] = json.RawMessage(`{"runtime":{"round":41}}`)
	posts := model.ObjectOf(curKeys["post"])
	p1 := model.ObjectOf(posts["p1"])
	p1["sent"] = json.RawMessage(`{"url":"u"}`)
	posts["p1"] = model.MustJSON(p1)
	next, err := nextBoard(cur, map[string]json.RawMessage{"system": model.MustJSON(system), "post": model.MustJSON(posts)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(next)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(out, &got)
	for k := range got {
		if strings.Contains(k, ".") {
			t.Errorf("flat decoy key %q in %s", k, out)
		}
	}
	if string(got["system"]) != `{"ebac":{"runtime":{"round":41}}}` {
		t.Errorf("system = %s", got["system"])
	}
	if string(got["post"]) != `{"p1":{"body":"x","pr":"a/b#1","sent":{"url":"u"}}}` {
		t.Errorf("post = %s", got["post"])
	}
}
