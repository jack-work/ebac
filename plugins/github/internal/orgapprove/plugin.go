package orgapprove

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"github.com/gluck/ebac/sdk/grants"
	"os"
	"time"

	"github.com/gluck/ebac/plugins/github/internal/approve"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// Plugin serves org-approve (in the ebac-approve binary). nil fields are the
// real ones.
type Plugin struct {
	Reader Reader
	Writer Writer
	Filer  Filer
	Grants func() ([]grants.Grant, error)
	Now    func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound && req.Op != plugin.OpCommit {
		return nil, fmt.Errorf("op %q not supported: org-approve has no seat, brief or outbox", req.Op)
	}
	w := &req.Critic
	if err := ghcfg.CheckOrgApprove(w.OrgApprove); err != nil {
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
		fl = grants.Filer{}
	}
	gs := pl.Grants
	if gs == nil {
		gs = func() ([]grants.Grant, error) { return grants.ReadGrants(grants.GrantsFile) }
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	if w.Runtime.OrgApprove == nil {
		w.Runtime.OrgApprove = &model.OrgApproveState{}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	o, err := Run(ctx, rd, wr, fl, Pass{Watch: *w.OrgApprove, State: w.Runtime.OrgApprove, Grants: gs, Now: now(), DryRun: req.DryRun,
		Op: req.Op, Token: req.CommitToken})
	rep.Commit = o.Commit && req.Op == plugin.OpRound && !req.DryRun
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
