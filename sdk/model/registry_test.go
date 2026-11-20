package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `ebac config check`, arm by arm: equal sets give no line; a declared critic with no live one, a live one
// undeclared, a moved form and a differing critic field each give exactly their line; a key absent on one side and
// empty on the other is equal. RED if an arm goes silent (the proof would pass while the sets differ).
func TestCheckRegistrations(t *testing.T) {
	c := func(s string) json.RawMessage { return json.RawMessage(s) }
	regs := map[string]Registration{
		"a": {Form: "@a", Critic: c(`{"name":"a","every_sec":300,"write":false}`)},
		"b": {Form: "@b", Critic: c(`{"name":"b","every_sec":60}`)},
		"c": {Form: "@c", Critic: c(`{"name":"c"}`)},
		"d": {Form: "@d1", Critic: c(`{"name":"d"}`)},
	}
	live := map[string]Registration{
		"a": {Form: "@a", Critic: c(`{"name":"a","every_sec":300}`)},
		"b": {Form: "@b", Critic: c(`{"name":"b","every_sec":120}`)},
		"d": {Form: "@d2", Critic: c(`{"name":"d"}`)},
		"e": {Form: "@e", Critic: c(`{"name":"e"}`)},
	}
	want := []string{
		"b: critic.every_sec differs",
		"c: declared (form @c) but no live critic",
		"d: form @d1 declared, @d2 live",
		"e: live (form @e) but undeclared",
	}
	if got := CheckRegistrations(regs, live); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := CheckRegistrations(map[string]Registration{"a": regs["a"]}, map[string]Registration{"a": live["a"]}); len(got) != 0 {
		t.Fatalf("equal sets: %v", got)
	}
}

func TestLoadRegistrations(t *testing.T) {
	dir := t.TempDir()
	if r, d, err := LoadRegistrations(filepath.Join(dir, "absent")); r != nil || d || err != nil {
		t.Fatalf("absent: %v %v %v", r, d, err)
	}
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":{}}`), 0o600)
	if _, d, err := LoadRegistrations(p); d || err != nil {
		t.Fatalf("no key: declared=%v %v", d, err)
	}
	_ = os.WriteFile(p, []byte(`{"registrations":{"a":{"form":"@a","critic":{"name":"a"}}}}`), 0o600)
	if r, d, err := LoadRegistrations(p); !d || err != nil || r["a"].Form != "@a" {
		t.Fatalf("declared: %v %v %v", r, d, err)
	}
	_ = os.WriteFile(p, []byte(`{"registrations":`), 0o600)
	if _, _, err := LoadRegistrations(p); err == nil {
		t.Fatal("a torn config loaded")
	}
}

// Two declarations differing ONLY in approve (authority-from-flake): the check names critic.approve, both ways.
// RED if diffKeys skips approve (review grok-4.7 on 01a305a).
func TestCheckRegistrationsOnlyApproveDiffers(t *testing.T) {
	for _, c := range []struct{ decl, live string }{
		{`{"name":"a","approve":true}`, `{"name":"a"}`},
		{`{"name":"a"}`, `{"name":"a","approve":true}`},
	} {
		got := CheckRegistrations(map[string]Registration{"a": {Form: "@a", Critic: json.RawMessage(c.decl)}},
			map[string]Registration{"a": {Form: "@a", Critic: json.RawMessage(c.live)}})
		if strings.Join(got, "|") != "a: critic.approve differs" {
			t.Errorf("%s vs %s: %v", c.decl, c.live, got)
		}
	}
}
