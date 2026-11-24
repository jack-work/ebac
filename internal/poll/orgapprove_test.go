package poll

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

var orgCritic = &model.Critic{Name: "oa", Source: model.SourceOrgApprove, InputsForm: "@board",
	OrgApprove: &model.OrgApproveWatch{PingTo: "@000000a4"}}

// org-approve writes only q.oa-* asker fields on its board, pings only its
// configured role, and sets no key on its own role form.
func TestValidateOrgApproveWrites(t *testing.T) {
	ok := plugin.FormWrite{Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-115803-aaaaaaa":{"ask":"a","context":"c","since":"t","default":"d","order":1,"status":"open","resolved":"r"}}`)}}
	if err := ValidateForms(orgCritic, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err != nil {
		t.Fatalf("an ask refused: %v", err)
	}
	for name, fw := range map[string]plugin.FormWrite{
		"other key":        {Inputs: true, Set: map[string]json.RawMessage{"about": raw(`"x"`)}},
		"other q id":       {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"go-1":{"status":"done"}}`)}},
		"answer":           {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-1-aaaaaaa":{"answer":"approve"}}`)}},
		"elaborate":        {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-1-aaaaaaa":{"elaborate":"x"}}`)}},
		"answered status":  {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-1-aaaaaaa":{"status":"answered"}}`)}},
		"not object":       {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`[1]`)}},
		"unset":            {Inputs: true, Unset: []string{"q"}},
		"child form":       {Key: "x", Set: map[string]json.RawMessage{"a": raw(`1`)}},
		"role form":        {Set: map[string]json.RawMessage{"q": raw(`{}`)}},
		"head not 7 hex":   {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-1-zzzzzzz":{"status":"open"}}`)}},
		"dotted id decoy":  {Inputs: true, Set: map[string]json.RawMessage{"q": raw(`{"oa-1-aaaaaaa.answer":{"status":"open"}}`)}},
		"retire the board": {Inputs: true, Retire: true},
	} {
		if err := ValidateForms(orgCritic, &plugin.Reply{Forms: []plugin.FormWrite{ok, fw}}); err == nil || !strings.Contains(err.Error(), "reply refused") {
			t.Errorf("%s: not refused: %v", name, err)
		}
	}
	noBoard := *orgCritic
	noBoard.InputsForm = ""
	if err := ValidateForms(&noBoard, &plugin.Reply{Forms: []plugin.FormWrite{ok}}); err == nil {
		t.Error("a write without a board was not refused")
	}
	if err := ValidatePings(orgCritic, &plugin.Reply{Pings: []plugin.Ping{{To: "@000000a4", Text: "asked"}}}); err != nil {
		t.Fatalf("the configured ping refused: %v", err)
	}
	if err := ValidatePings(orgCritic, &plugin.Reply{Pings: []plugin.Ping{{To: "@aaaaaaaa", Text: "asked"}}}); err == nil {
		t.Error("a ping to another role was not refused")
	}
	if err := SourceKeysOnly(model.SourceOrgApprove, &plugin.Reply{Set: map[string]json.RawMessage{"x": raw(`1`)}}); err == nil {
		t.Error("a role-form key write was not refused")
	}
}

// The merge keeps the Boss's fields and every other asker's q; a q that is
// not an object is never clobbered; an unchanged write is no write.
func TestMergeAsksKeepsTheBossAndOtherAskers(t *testing.T) {
	cur := raw(`{"go-7":{"ask":"ops's","answer":"GO"},"oa-1-aaaaaaa":{"ask":"a","status":"open","answer":"approve","elaborate":"why"}}`)
	set, _ := mergeAsks(cur, map[string]map[string]json.RawMessage{
		"oa-1-aaaaaaa": {"status": raw(`"done"`), "resolved": raw(`"approved"`)},
		"oa-2-bbbbbbb": {"ask": raw(`"b"`), "status": raw(`"open"`)},
	})
	var q map[string]map[string]any
	if err := json.Unmarshal(set["q"], &q); err != nil {
		t.Fatal(err)
	}
	if q["go-7"]["answer"] != "GO" || q["oa-1-aaaaaaa"]["answer"] != "approve" || q["oa-1-aaaaaaa"]["elaborate"] != "why" {
		t.Fatalf("lost a field: %v", q)
	}
	if q["oa-1-aaaaaaa"]["status"] != "done" || q["oa-1-aaaaaaa"]["ask"] != "a" || q["oa-2-bbbbbbb"]["ask"] != "b" {
		t.Fatalf("merge: %v", q)
	}
	if s, _ := mergeAsks(set["q"], map[string]map[string]json.RawMessage{"oa-2-bbbbbbb": {"ask": raw(`"b"`)}}); s != nil {
		t.Fatalf("an unchanged write wrote %s", s["q"])
	}
	if s, _ := mergeAsks(raw(`"text"`), map[string]map[string]json.RawMessage{"oa-2-bbbbbbb": {"ask": raw(`"b"`)}}); s != nil {
		t.Fatal("clobbered a q that is not an object")
	}
	if s, _ := mergeAsks(nil, map[string]map[string]json.RawMessage{"oa-2-bbbbbbb": {"ask": raw(`"b"`)}}); s == nil {
		t.Fatal("a board without q got no ask")
	}
}
