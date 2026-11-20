package model

// From review 7d507c69 of 7380f82, adopted. Two groups:
//  - Rejecting cases: a non-zero value on ONE side must give "critic.<k> differs". These pass at 7380f82 and kill
//    the surviving mutants M1-M5 (live-only keys ignored; true/string/list/object treated as absent).
//  - False passes: CheckRegistrations reports no line while the critic ebac decodes differs. RED at 7380f82.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func reviewCheck(decl, live string) []string {
	return CheckRegistrations(
		map[string]Registration{"a": {Form: "@a", Critic: json.RawMessage(decl)}},
		map[string]Registration{"a": {Form: "@a", Critic: json.RawMessage(live)}},
	)
}

func TestReviewNonZeroOnOneSideDiffers(t *testing.T) {
	for _, c := range []struct{ name, decl, live, want string }{
		{"declared write=true, live absent", `{"name":"a","write":true}`, `{"name":"a"}`, "a: critic.write differs"},
		{"live-only write=true", `{"name":"a"}`, `{"name":"a","write":true}`, "a: critic.write differs"},
		{"live-only source", `{"name":"a"}`, `{"name":"a","source":"teams-chat"}`, "a: critic.source differs"},
		{"live-only prs", `{"name":"a","prs":[]}`, `{"name":"a","prs":[{"owner":"o","repo":"r","number":1}]}`, "a: critic.prs differs"},
		{"live-only chat", `{"name":"a"}`, `{"name":"a","chat":{"id":"19:x"}}`, "a: critic.chat differs"},
	} {
		if got := strings.Join(reviewCheck(c.decl, c.live), "|"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Go's decoder matches keys case-insensitively, last one wins; the check compares exact key names and drops zero
// values, so a zero-valued case variant is invisible to it but empties the field ebac runs with.
func TestReviewCaseFoldedZeroKeyIsAFalsePass(t *testing.T) {
	for _, c := range []struct{ name, decl, live string }{
		{"PRS empties prs", `{"name":"a","prs":[{"owner":"o","repo":"r","number":1}]}`, `{"name":"a","prs":[{"owner":"o","repo":"r","number":1}],"PRS":[]}`},
		{"Write clears write", `{"name":"a","write":true}`, `{"name":"a","write":true,"Write":false}`},
	} {
		var d, l Critic
		_ = json.Unmarshal([]byte(c.decl), &d)
		_ = json.Unmarshal([]byte(c.live), &l)
		if reflect.DeepEqual(d, l) {
			t.Fatalf("%s: premise: decoded critics are equal", c.name)
		}
		if got := reviewCheck(c.decl, c.live); len(got) == 0 {
			t.Errorf("%s: decoded critics differ (prs %d vs %d, write %v vs %v) but config check prints no line",
				c.name, len(d.PRs), len(l.PRs), d.Write, l.Write)
		}
	}
}

// An empty object equals absent to the check, but ebac decodes it as a non-nil pointer (brief.go:344 `w.Chat != nil`,
// bandstate/plugin.go:32 `w.Band == nil`).
func TestReviewEmptyObjectVsAbsentIsAFalsePass(t *testing.T) {
	var d, l Critic
	decl, live := `{"name":"a","chat":{}}`, `{"name":"a"}`
	_ = json.Unmarshal([]byte(decl), &d)
	_ = json.Unmarshal([]byte(live), &l)
	if (d.Chat == nil) == (l.Chat == nil) {
		t.Fatal("premise: both nil or both set")
	}
	if got := reviewCheck(decl, live); len(got) == 0 {
		t.Errorf("chat {} vs absent: decoded Chat nil=%v vs nil=%v but config check prints no line", d.Chat == nil, l.Chat == nil)
	}
}
