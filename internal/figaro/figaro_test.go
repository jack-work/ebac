package figaro

import (
	"strings"
	"testing"
)

func TestSetJSONRefusesDottedKeys(t *testing.T) {
	// Guards the measured figaro hazard: a dotted write into a subtree that
	// was written as a literal creates a flat decoy key, and a later unset
	// "succeeds" against the decoy while the real value stands.
	f := NewFigaro("/nonexistent-figaro")
	err := f.SetJSON(t.Context(), "@x", "prs.foo", 1)
	if err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("dotted key was not refused: %v", err)
	}
}
