package poll

import (
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/figaro"
)

// Succession thresholds ship in the binary, so they are testable without a
// config file present — which is the whole point of moving them here.
func TestSuccessionPolicyIsShippedAndOrdered(t *testing.T) {
	p := DefaultSuccession()
	if p.StandbyAt >= p.RecastAt {
		t.Fatalf("standby %.2f must come before recast %.2f", p.StandbyAt, p.RecastAt)
	}
	if p.StandbyAt != 0.70 || p.RecastAt != 0.80 {
		t.Fatalf("thresholds drifted: %.2f/%.2f", p.StandbyAt, p.RecastAt)
	}
	if p.ForkTurn > 2 {
		t.Fatalf("fork turn %d is not early; a late fork inherits the fill it is meant to escape", p.ForkTurn)
	}
	if len(p.DivisibleTasks) == 0 {
		t.Fatal("standby has no read-only warm-up tasks")
	}
	for _, task := range p.DivisibleTasks {
		l := strings.ToLower(task)
		if strings.Contains(l, "reply to") && !strings.Contains(l, "do not") {
			t.Fatalf("standby task lets a second holder act: %q", task)
		}
	}
}

// An unknown context_limit must not read as an empty context. Reporting 0%
// for a seat that may be full is the direction that loses the assignment.
func TestUnknownContextLimitIsAnErrorNotZeroFill(t *testing.T) {
	f := figaro.NewFigaro("/nonexistent-figaro")
	_, _, _, err := f.AriaFill(t.Context(), "deadbeef")
	if err == nil {
		t.Fatal("a failed status read reported a usable fill")
	}
}

func TestSuccessionDecision(t *testing.T) {
	p := DefaultSuccession()
	for _, tc := range []struct {
		name         string
		fill         float64
		standby      string
		standbyAlive bool
		want         succAction
	}{
		{"idle", 0.34, "", false, succNone},
		{"just below standby", 0.699, "", false, succNone},
		{"at standby", 0.70, "", false, succMint},
		{"above standby, standby exists", 0.75, "abc123", true, succNone},
		{"at recast with a live standby", 0.80, "abc123", true, succRecast},
		// The one that would otherwise hang forever: full, but the standby
		// was reaped. Re-mint rather than wait for an aria that is gone.
		{"at recast, standby reaped", 0.85, "abc123", false, succRemint},
		// And full with no standby at all still mints one rather than
		// falling through to none.
		{"at recast, no standby", 0.85, "", false, succMint},
	} {
		if got := successionAction(tc.fill, tc.standby, tc.standbyAlive, p); got != tc.want {
			t.Errorf("%s: fill=%.3f standby=%q alive=%v → %s, want %s",
				tc.name, tc.fill, tc.standby, tc.standbyAlive, got, tc.want)
		}
	}
}
