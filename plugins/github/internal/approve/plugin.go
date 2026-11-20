package approve

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"github.com/gluck/ebac/sdk/grants"
	"os"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
	"github.com/gluck/ebac/sdk/tomb"
)

// Plugin serves approve-requests. GH, Teams, Grants and Now replace the real
// ones in tests; nil means the real ones.
type Plugin struct {
	GH     *GH
	Teams  Teams
	Grants func() ([]grants.Grant, error)
	Now    func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound && req.Op != plugin.OpCommit {
		return nil, fmt.Errorf("op %q not supported: approve-requests has no seat, brief or outbox", req.Op)
	}
	w := &req.Critic
	if err := ghcfg.CheckApprove(w.Requests); err != nil {
		return nil, fmt.Errorf("%s: %w", w.Name, err)
	}
	gh := pl.GH
	if gh == nil {
		bin := req.GH
		if bin == "" {
			bin = "gh"
		}
		gh = &GH{Bin: bin, Cred: forge.NewCred(os.Getenv("EBAC_BAND_CRED"), forge.CredGitHub), Timeout: 90 * time.Second}
	}
	var teams Teams = pl.Teams
	if teams == nil {
		bin := os.Getenv("EBAC_TOMB")
		if bin == "" {
			bin = "tomb"
		}
		teams = &tomb.Tomb{Bin: bin, Timeout: tomb.TeamsTombWait, Cred: forge.NewCred(os.Getenv("EBAC_BAND_CRED"), forge.CredTeams)}
	}
	grantsFn := pl.Grants
	if grantsFn == nil {
		grantsFn = func() ([]grants.Grant, error) { return grants.ReadGrants(grants.GrantsFile) }
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	var reqs map[string]json.RawMessage
	if raw, ok := req.Inputs["requests"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &reqs); err != nil {
			reqs = nil // not an object: nothing to decide
		}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	o, err := Run(ctx, gh, teams, Pass{Watch: *w.Requests, Requests: reqs, Grants: grantsFn, Now: now(), DryRun: req.DryRun,
		Op: req.Op, Token: req.CommitToken, Posts: w.Runtime.ApprovePosts})
	if len(o.Posts) == 0 {
		o.Posts = nil
	}
	w.Runtime.ApprovePosts = o.Posts
	rep.Commit = o.Commit && req.Op == plugin.OpRound
	for _, l := range o.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if s, ok := any(gh).(interface{ Spent() Spend }); ok {
		rep.Spend = s.Spent().Budget()
	}
	if len(o.Results) > 0 {
		set := map[string]any{}
		for id, r := range o.Results {
			set[id] = map[string]any{"result": r}
		}
		rep.Forms = []plugin.FormWrite{{Inputs: true, Set: map[string]json.RawMessage{"requests": model.MustJSON(set)}}}
	}
	rep.Pings = o.Pings
	if err != nil {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	rep.Logf("%s: round %d %s: %d request(s) pending, %d decided, %d prepared/posted on record, %d ping(s)", w.Name, w.Runtime.Round, req.Op, len(reqs), len(o.Results), len(o.Posts), len(o.Pings))
	return rep, nil
}

// GHECred is band-cred's GHE credential (ghe-ebac) on socket sock: what an
// org-approve critic's GH runs with.
func GHECred(sock string) *forge.Cred { return forge.NewCred(sock, forge.CredGitHub) }
