package main

import (
	"reflect"
	"testing"

	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/plugin"
)

// The stamp key is registered as a grant, so the sandbox hides it from every
// plugin (no plugin's Needs names it).
func TestGrantsRegisterTheStampKey(t *testing.T) {
	t.Setenv("EBAC_STAMP_KEY", "/var/lib/ebac/.local/state/ebac-stamp/stamp.key")
	if got := grants(globals{})[plugin.GrantStampKey]; !reflect.DeepEqual(got, []string{identity.KeyPath()}) {
		t.Fatalf("stamp-key grant %v", got)
	}
	for _, p := range []string{"keel", "gh", "tomb", "approve", "bandhub", "roster"} {
		for _, g := range plugin.Needs(p, nil) {
			if g == plugin.GrantStampKey {
				t.Errorf("plugin %s needs the stamp key", p)
			}
		}
	}
}
