package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/bandstate"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// Plugin serves github-scout. GH, Hub and Now replace the forge, the band hub
// and the clock in tests; nil means the real ones.
type Plugin struct {
	GH  GitHub
	Hub Hub
	Now func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: the scout has no seat, no brief and no outbox", req.Op)
	}
	w := &req.Critic
	if w.Scout == nil || len(w.Scout.Authors) == 0 || !strings.Contains(w.Scout.Repo, "/") {
		return nil, fmt.Errorf("%s: a scout without authors and an owner/repo", w.Name)
	}
	gh := pl.GH
	if gh == nil {
		g := forge.NewGH(req.GH, req.Host)
		if w.Host != "" {
			g.Host = w.Host // the token follows the host
		}
		gh = g
	}
	hub := pl.Hub
	if hub == nil {
		hub = hubClient{&bandstate.Client{Socket: bandstate.DefaultSocket}}
	}
	now := time.Now
	if pl.Now != nil {
		now = pl.Now
	}
	var prev State
	if len(w.Runtime.Scout) > 0 {
		if err := json.Unmarshal(w.Runtime.Scout, &prev); err != nil {
			prev = State{}
		}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	r, err := Run(ctx, gh, hub, prev, Pass{Watch: *w.Scout, Inputs: req.Inputs, Board: req.Board, Now: now()})
	for _, l := range r.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if err != nil {
		// A round that could not read its source is a failure: last_success
		// (what the steward's health timer reads) does not move, and neither
		// does the cursor.
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	w.Runtime.Scout = model.MustJSON(r.State)
	rep.Set, rep.Unset, rep.Forms = r.Set, r.Unset, r.Forms
	rep.Logf("%s: round %d: %d GitHub call(s), %d entr(y/ies) written, %d removed, %d PR(s) and %d issue(s) tracked",
		w.Name, w.Runtime.Round, r.GHCalls, len(r.Set), len(r.Unset), len(r.State.PRs), len(r.State.Issues))
	return rep, nil
}

// hubClient reads the band hub as a metadata reader.
type hubClient struct{ c *bandstate.Client }

func (h hubClient) Jobs(ctx context.Context, since time.Time) ([]HubJob, error) {
	var jobs []HubJob
	q := url.Values{"since": {fmt.Sprint(since.UnixMilli())}, "sort": {"updated"}, "order": {"desc"}, "limit": {"2000"}}
	return jobs, h.c.Get(ctx, "/jobs?"+q.Encode(), &jobs)
}

// Tests reads GET /jobs/<id>/tests: an object carrying tests_run. Until the
// route exists (404), or while it answers null, the count is unknown, never a
// pass.
func (h hubClient) Tests(ctx context.Context, id int64) (int, bool, error) {
	var v any
	if err := h.c.Get(ctx, fmt.Sprintf("/jobs/%d/tests", id), &v); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return 0, false, nil
		}
		return 0, false, err
	}
	return testsRun(v)
}

// testsRun reads the tests route's answer: {"tests_run": n}; null, a missing
// key or a non-number is unknown.
func testsRun(v any) (int, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false, nil
	}
	f, ok := m["tests_run"].(float64)
	if !ok {
		return 0, false, nil
	}
	return int(f), true, nil
}
