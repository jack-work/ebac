package main

import (
	"reflect"
	"strings"
	"testing"
)

// The operator wrapper writes --caller=<aria> FIRST; nothing the user types
// can override it, swallow it (an unterminated --host) or add another.
func TestCallerComesOnlyFromTheWrappersFirstArgument(t *testing.T) {
	c, rest, err := splitCaller([]string{"--caller=" + strings.Repeat("a", 32), "verdict", "--host"})
	if err != nil || c != strings.Repeat("a", 32) || !reflect.DeepEqual(rest, []string{"verdict", "--host"}) {
		t.Fatalf("wrapper form: %q %v %v", c, rest, err)
	}
	for name, args := range map[string][]string{
		"user --caller after the wrapper's": {"--caller=" + strings.Repeat("a", 32), "verdict", "--caller", "b"},
		"user --caller= anywhere":           {"verdict", "--caller=b"},
		"user --caller first, no '='":       {"--caller", "b", "verdict"},
	} {
		if _, _, err := splitCaller(args); err == nil || !strings.Contains(err.Error(), "operator wrapper only") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c, rest, err := splitCaller([]string{"ls"}); err != nil || c != "" || !reflect.DeepEqual(rest, []string{"ls"}) {
		t.Fatalf("no caller: %q %v %v", c, rest, err)
	}
	if _, _, err := splitCaller([]string{"--caller=", "ls"}); err == nil {
		t.Fatal("empty --caller= accepted")
	}
}
