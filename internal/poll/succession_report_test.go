package poll

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

// Report mode (GO-20261104-ebac-narrow): with EBAC_SUCCESSION_REPORT_TO set, a seat past the standby threshold is
// REPORTED to that role once, never forked. Live: the report is a real figaro send to the role named by
// EBAC_TEST_REPORT_ROLE (the dry trigger that proves it reaches the maintainer role); the seat is that role's holder.
func TestLiveSuccessionReportsInsteadOfForking(t *testing.T) {
	f := liveOrSkip(t)
	role := os.Getenv("EBAC_TEST_REPORT_ROLE")
	if role == "" {
		t.Skip("set EBAC_TEST_REPORT_ROLE=@<role> (the report is a real send to it)")
	}
	ctx := context.Background()
	st, w, _ := liveCritic(t, f, false)
	rf, err := f.Form(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := rf["target-aria"].(string)
	if holder == "" {
		t.Fatalf("%s has no target-aria", role)
	}
	if err := f.SetJSON(ctx, w.FormID, "target-aria", holder); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EBAC_SUCCESSION_REPORT_TO", role)
	orig := successionPolicy
	successionPolicy = func() SuccessionPolicy { p := DefaultSuccession(); p.StandbyAt, p.RecastAt = 0, 2; return p }
	t.Cleanup(func() { successionPolicy = orig })
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out}

	p.SuperviseSuccession(ctx, w) // round 1: report
	c, _ := st.LoadCritic(ctx, w.Name)
	if c.Runtime.SuccessionReported != holder || c.Runtime.Standby != "" {
		t.Fatalf("round 1: reported=%q standby=%q; want reported=%s and NO standby (no fork)\n%s",
			c.Runtime.SuccessionReported, c.Runtime.Standby, holder, out.String())
	}
	if !strings.Contains(out.String(), "reported to "+role+" (no fork)") {
		t.Fatalf("no report line: %s", out.String())
	}
	first := out.Len()
	p.SuperviseSuccession(ctx, c) // round 2: once per holder: no second send
	if strings.Contains(out.String()[first:], "reported to") {
		t.Fatalf("reported twice for the same holder: %s", out.String())
	}
	form, _ := f.Form(ctx, w.FormID)
	state, _ := form["succession_state"].(map[string]any)
	if state["stage"] != "report-mode" {
		t.Fatalf("succession_state.stage = %v; want report-mode", state["stage"])
	}
	t.Logf("report sent to %s for holder %s; state note: %v", role, holder, state["note"])
	_ = model.Critic{}
}
