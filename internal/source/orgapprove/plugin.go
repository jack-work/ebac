package orgapprove

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/approve"
)

// Plugin serves org-approve (in the ebac-approve binary). nil fields are the
// real ones.
type Plugin struct {
	Reader Reader
	Writer Writer
	Filer  Filer
	Grants func() ([]approve.Grant, error)
	Now    func() time.Time
}

// CheckWatch refuses an incomplete configuration.
func CheckWatch(w *model.OrgApproveWatch) error {
	switch {
	case w == nil:
		return fmt.Errorf("no org-approve configuration")
	case len(w.Logins) == 0:
		return fmt.Errorf("logins: want at least one GHE login")
	case w.Grant == "" || strings.ContainsAny(w.Grant, "*?"):
		return fmt.Errorf("grant %q: want one exact credential name", w.Grant)
	case w.Principal == "":
		return fmt.Errorf("principal is required")
	case w.Approver == "" || strings.HasPrefix(w.Approver, "flake:"):
		return fmt.Errorf("approver %q: want a person (band-grants approved_by)", w.Approver)
	case w.PingTo != "" && !strings.HasPrefix(w.PingTo, "@"):
		return fmt.Errorf("ping_to %q: want a role form (@id)", w.PingTo)
	}
	for _, l := range w.Logins {
		if !ValidLogin(l) {
			return fmt.Errorf("login %q: want a GHE login", l)
		}
	}
	return nil
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: org-approve has no seat, brief or outbox", req.Op)
	}
	w := &req.Critic
	if err := CheckWatch(w.OrgApprove); err != nil {
		return nil, fmt.Errorf("%s: %w", w.Name, err)
	}
	rd, wr := pl.Reader, pl.Writer
	if rd == nil || wr == nil {
		bin := req.GH
		if bin == "" {
			bin = "gh"
		}
		gh := &approve.GH{Bin: bin, Cred: approve.GHECred(os.Getenv("EBAC_BAND_CRED")), Timeout: 90 * time.Second}
		if rd == nil {
			rd = gh
		}
		if wr == nil {
			wr = gh
		}
	}
	var fl Filer = pl.Filer
	if fl == nil {
		fl = approve.Filer{}
	}
	grants := pl.Grants
	if grants == nil {
		grants = func() ([]approve.Grant, error) { return approve.ReadGrants(approve.GrantsFile) }
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	if w.Runtime.OrgApprove == nil {
		w.Runtime.OrgApprove = &model.OrgApproveState{}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	o, err := Run(ctx, rd, wr, fl, Pass{Watch: *w.OrgApprove, State: w.Runtime.OrgApprove, Grants: grants, Now: now(), DryRun: req.DryRun})
	for _, l := range o.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if s, ok := rd.(interface{ Spent() approve.Spend }); ok {
		rep.Spend = s.Spent().Budget()
	}
	if len(o.Q) > 0 && !req.DryRun {
		rep.Forms = []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"q": model.MustJSON(o.Q)}}}
	}
	if !req.DryRun {
		rep.Pings = o.Pings
	}
	if err != nil {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	return rep, nil
}
