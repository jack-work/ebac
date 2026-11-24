package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const child = `{"form":{"praxis":{"topic":"T","k":1,"n":1,"parent":1000,"summary":"First piece","delivers":"one outcome","status":"in-progress"}},"parent":{"number":1000,"open":true,"status":"split"}}`

func call(t *testing.T, verb, in string) (int, map[string]string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{verb}, strings.NewReader(in), &out, &errb)
	m := map[string]string{}
	_ = json.Unmarshal(out.Bytes(), &m)
	return code, m
}

func TestValidateCleanExitsZero(t *testing.T) {
	if code, m := call(t, "validate", child); code != 0 {
		t.Fatalf("clean form: exit %d %v", code, m)
	}
}

func TestValidateRefusalExitsThreeWithRuleAndAlarm(t *testing.T) {
	in := strings.Replace(child, `"parent":1000,`, ``, 1)
	code, m := call(t, "validate", in)
	if code != 3 || m["rule"] != "no_parent" || m["alarm"] != "sync.alarm.praxis.no_parent" {
		t.Fatalf("want exit 3 no_parent, got %d %v", code, m)
	}
}

func TestRenderPrintsTitleAndBody(t *testing.T) {
	code, m := call(t, "render", child)
	if code != 0 || m["title"] != "[T][1-1] First piece" || !strings.HasPrefix(m["body"], "Part of #1000\nStack: 1 of 1\n") {
		t.Fatalf("render: %d %v", code, m)
	}
}

func TestBadInputExitsTwo(t *testing.T) {
	if code, _ := call(t, "validate", "{not json"); code != 2 {
		t.Fatalf("bad input: exit %d", code)
	}
	if code, _ := call(t, "nope", child); code != 2 {
		t.Fatalf("bad verb: exit %d", code)
	}
}
