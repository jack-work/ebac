package approve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/tomb"
)

// Plugin serves approve-requests. GH, Teams, Grants and Now replace the real
// ones in tests; nil means the real ones.
type Plugin struct {
	GH     *GH
	Teams  Teams
	Grants func() ([]Grant, error)
	Now    func() time.Time
}

// CheckWatch refuses an incomplete configuration.
func CheckWatch(w *model.ApproveWatch) error {
	switch {
	case w == nil:
		return fmt.Errorf("no approve configuration")
	case !strings.HasPrefix(w.Chat, "19:"):
		return fmt.Errorf("chat %q: want a Teams chat id (19:...)", w.Chat)
	case w.ChatAlias == "":
		return fmt.Errorf("chat alias is required")
	case !strings.HasPrefix(w.AskFrom, "8:orgid:"):
		return fmt.Errorf("ask_from %q: want a Teams sender id (8:orgid:<guid>)", w.AskFrom)
	case w.Grant == "" || strings.ContainsAny(w.Grant, "*?"):
		return fmt.Errorf("grant %q: want one exact credential name", w.Grant)
	case w.Principal == "":
		return fmt.Errorf("principal is required")
	case w.PingTo != "" && !strings.HasPrefix(w.PingTo, "@"):
		return fmt.Errorf("ping_to %q: want a role form (@id)", w.PingTo)
	}
	return nil
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: approve-requests has no seat, brief or outbox", req.Op)
	}
	w := &req.Critic
	if err := CheckWatch(w.Requests); err != nil {
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
	grants := pl.Grants
	if grants == nil {
		grants = func() ([]Grant, error) { return ReadGrants(GrantsFile) }
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
	o, err := Run(ctx, gh, teams, Pass{Watch: *w.Requests, Requests: reqs, Grants: grants, Now: now(), DryRun: req.DryRun})
	for _, l := range o.Log {
		rep.Logf("%s: %s", w.Name, l)
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
	rep.Logf("%s: round %d: %d request(s) pending, %d decided, %d ping(s)", w.Name, w.Runtime.Round, len(reqs), len(o.Results), len(o.Pings))
	return rep, nil
}
