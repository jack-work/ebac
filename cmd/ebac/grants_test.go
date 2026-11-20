package main

import (
	"github.com/gluck/ebac/internal/pluginrun"
	"reflect"
	"testing"

	"github.com/gluck/ebac/internal/identity"
)

// The stamp key is registered as a grant, so the sandbox hides it from every
// plugin (no plugin's Needs names it).
func TestGrantsRegisterTheStampKey(t *testing.T) {
	t.Setenv("EBAC_STAMP_KEY", "/var/lib/ebac/.local/state/ebac-stamp/stamp.key")
	if got := pluginGrants(globals{})[pluginrun.GrantStampKey]; !reflect.DeepEqual(got, []string{identity.KeyPath()}) {
		t.Fatalf("stamp-key grant %v", got)
	}
	for _, p := range []string{"keel", "gh", "tomb", "approve", "bandhub", "roster"} {
		for _, g := range pluginrun.Needs(p, nil) {
			if g == pluginrun.GrantStampKey {
				t.Errorf("plugin %s needs the stamp key", p)
			}
		}
	}
}
