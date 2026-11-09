package trigger

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func mustParse(t *testing.T, s string) Rules {
	t.Helper()
	rs, err := Parse(json.RawMessage(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return rs
}

// tierOf runs one event through rules: "wake", "record", "drop", or "keep:<tier>".
func tierOf(rs Rules, e model.Event) string {
	kept, dropped := rs.Apply([]model.Event{e})
	if len(dropped) == 1 {
		return "drop"
	}
	switch kept[0].Tier {
	case model.TierWake:
		return "wake"
	case model.TierRecord:
		return "record"
	}
	return "none"
}

var (
	boss    = model.Event{Kind: "teams_new", Author: "Jack Kelliher", AuthorID: OperatorSender, Text: "boss note"}
	akshay  = model.Event{Kind: "teams_new", Author: "Akshay", AuthorID: "8:orgid:00000000-0000-0000-0000-00000000000a", Text: "Please LOOK at the build"}
	comment = model.Event{Kind: model.EvCommentNew, Author: "chiqun", Text: "first line\nsecond line says SHIP", Tier: model.TierRecord}
)

func TestFromMatchesNameIDGuidAndOperator(t *testing.T) {
	for _, from := range []string{"operator", "OPERATOR", "Jack Kelliher", "jack kelliher", OperatorSender,
		strings.ToUpper(OperatorSender), "bc3cb0f4-1239-4ec7-a528-4913522f79c5"} {
		rs := mustParse(t, `[{"from":`+string(mustJSON(from))+`,"action":"record"},{"default":true,"action":"wake"}]`)
		if got := tierOf(rs, boss); got != "record" {
			t.Errorf("from %q: boss -> %s; want record", from, got)
		}
		if got := tierOf(rs, akshay); got != "wake" {
			t.Errorf("from %q: control akshay -> %s; want wake", from, got)
		}
	}
}

func TestFromListAndNotFrom(t *testing.T) {
	rs := mustParse(t, `[{"from":["nobody","Akshay"],"action":"drop"},{"default":true,"action":"record"}]`)
	if tierOf(rs, akshay) != "drop" || tierOf(rs, boss) != "record" {
		t.Fatalf("from list: akshay %s boss %s", tierOf(rs, akshay), tierOf(rs, boss))
	}
	rs = mustParse(t, `[{"not_from":"operator","action":"wake"},{"default":true,"action":"record"}]`)
	if tierOf(rs, akshay) != "wake" || tierOf(rs, boss) != "record" {
		t.Fatalf("not_from: akshay %s boss %s", tierOf(rs, akshay), tierOf(rs, boss))
	}
}

func TestContainsIsCaseInsensitiveOverFullText(t *testing.T) {
	rs := mustParse(t, `[{"contains":["nothing","ship"],"action":"wake"},{"default":true,"action":"record"}]`)
	if got := tierOf(rs, comment); got != "wake" {
		t.Fatalf("contains on the SECOND line -> %s; want wake (full text, not the first line)", got)
	}
	if got := tierOf(rs, akshay); got != "record" {
		t.Fatalf("control: no word -> %s; want record", got)
	}
	// Detail stands in when a source gave no Text.
	if got := tierOf(rs, model.Event{Detail: "ship it"}); got != "wake" {
		t.Fatalf("Detail fallback -> %s", got)
	}
}

func TestRegexAndKind(t *testing.T) {
	rs := mustParse(t, `[{"regex":"(?i)\\blook\\b","kind":"teams_new","action":"wake"},{"default":true,"action":"drop"}]`)
	if got := tierOf(rs, akshay); got != "wake" {
		t.Fatalf("regex+kind -> %s", got)
	}
	other := akshay
	other.Kind = "teams_edited"
	if got := tierOf(rs, other); got != "drop" {
		t.Fatalf("kind must also match (AND): -> %s", got)
	}
	if got := tierOf(rs, boss); got != "drop" {
		t.Fatalf("control: regex miss -> %s", got)
	}
}

func TestFirstMatchWinsAndKeep(t *testing.T) {
	rs := mustParse(t, `[{"from":"chiqun","action":"keep"},{"contains":"ship","action":"wake"},{"default":true,"action":"drop"}]`)
	if got := tierOf(rs, comment); got != "record" {
		t.Fatalf("keep must leave the source tier (record) and stop there: -> %s", got)
	}
	rs = mustParse(t, `[{"default":true,"action":"keep"}]`)
	w := comment
	w.Tier = model.TierWake
	if tierOf(rs, comment) != "record" || tierOf(rs, w) != "wake" {
		t.Fatal("default keep changed a source tier")
	}
}

// The order cheroot set: a figaro-sent message (disclosure footer, under the Boss's account) wakes even though
// from=operator would record it.
func TestTeamsDefaultsDisclosureBeatsOperator(t *testing.T) {
	rs := Teams()
	sent := boss
	sent.Text = "build is red\n\nAgent-Authored on behalf of Jack Kelliher."
	if got := tierOf(rs, sent); got != "wake" {
		t.Fatalf("disclosed agent message -> %s; want wake", got)
	}
	sent.Text = "x\nSENT BY FIGARO, Jack Kelliher's automation."
	if got := tierOf(rs, sent); got != "wake" {
		t.Fatalf("per-message disclosure -> %s; want wake", got)
	}
	if got := tierOf(rs, boss); got != "record" {
		t.Fatalf("boss -> %s; want record", got)
	}
	if got := tierOf(rs, akshay); got != "wake" {
		t.Fatalf("akshay -> %s; want wake", got)
	}
	del := akshay
	del.Kind = "teams_deleted"
	if got := tierOf(rs, del); got != "record" {
		t.Fatalf("deletion -> %s; want record", got)
	}
	// What add-* writes round-trips through Parse.
	if _, err := Parse(rs.JSON()); err != nil {
		t.Fatalf("teams defaults do not parse: %v", err)
	}
	if _, err := Parse(SourceDefault().JSON()); err != nil {
		t.Fatalf("source default does not parse: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for in, want := range map[string]string{
		``:                                    "empty",
		`[]`:                                  "empty rule set",
		`{"default":true}`:                    "not a rule list",
		`[{"from":"a","action":"wake"}]`:      "last rule must be",
		`[{"default":true}]`:                  "action is required",
		`[{"default":true,"action":"shout"}]`: "unknown action",
		`[{"action":"wake"},{"default":true,"action":"record"}]`:             "no match fields",
		`[{"default":true,"action":"wake"},{"from":"a","action":"record"}]`:  "must be last",
		`[{"default":true,"from":"a","action":"wake"}]`:                      "takes no match fields",
		`[{"regex":"(","action":"wake"},{"default":true,"action":"record"}]`: "regex",
		`[{"form":"a","action":"wake"},{"default":true,"action":"record"}]`:  "unknown field",
		`[{"from":7,"action":"wake"},{"default":true,"action":"record"}]`:    "string or a list",
	} {
		_, err := Parse(json.RawMessage(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%s) = %v; want an error containing %q", in, err, want)
		}
	}
	// Control: the same shapes, fixed, parse; and a value stored as a JSON string is unwrapped.
	mustParse(t, `[{"from":"a","action":"wake"},{"default":true,"action":"record"}]`)
	mustParse(t, string(mustJSON(`[{"default":true,"action":"wake"}]`)))
}

func TestMigrateQuietDropsOperatorKeepsOthers(t *testing.T) {
	rs := MigrateQuiet([]string{"Jack Kelliher", " Liang Shi ", "bc3cb0f4-1239-4ec7-a528-4913522f79c5", "operator", ""}, "Jack Kelliher")
	b := string(rs.JSON())
	if strings.Contains(b, "Jack") || strings.Contains(b, "bc3cb0f4") {
		t.Fatalf("operator survived migration: %s", b)
	}
	liang := akshay
	liang.Author, liang.AuthorID = "Liang Shi", ""
	if got := tierOf(rs, liang); got != "record" {
		t.Fatalf("migrated quiet sender -> %s: %s", got, b)
	}
	if tierOf(rs, boss) != "record" || tierOf(rs, akshay) != "wake" {
		t.Fatalf("migrated set lost the defaults: %s", b)
	}
	if len(MigrateQuiet([]string{"Jack Kelliher"}, "Jack Kelliher")) != len(Teams()) {
		t.Fatal("an operator-only quiet list must migrate to exactly the defaults")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
