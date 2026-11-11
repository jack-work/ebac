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
	r, err := Run(ctx, gh, hub, prev, Pass{Watch: *w.Scout, Inputs: req.Inputs, Board: req.Board, Now: now(), OwnKeys: req.OwnKeys})
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
	rep.Set, rep.Unset, rep.Forms, rep.Pings = r.Set, r.Unset, r.Forms, r.Pings
	rep.Logf("%s: round %d: %d GitHub call(s), %d entr(y/ies) written, %d removed, %d PR(s) and %d issue(s) tracked, %d ping(s), %d queued",
		w.Name, w.Runtime.Round, r.GHCalls, len(r.Set), len(r.Unset), len(r.State.PRs), len(r.State.Issues), len(r.Pings), len(r.State.Queue))
	return rep, nil
}

// hubClient reads the band hub as a metadata reader.
type hubClient struct{ c *bandstate.Client }

func (h hubClient) Jobs(ctx context.Context, since time.Time) ([]HubJob, error) {
	var jobs []HubJob
	q := url.Values{"since": {fmt.Sprint(since.UnixMilli())}, "sort": {"updated"}, "order": {"desc"}, "limit": {"2000"}}
	return jobs, h.c.Get(ctx, "/jobs?"+q.Encode(), &jobs)
}

// Tests reads GET /jobs/<id>/tests: tests_run and crates_checked. Until the
// route exists (404), each count is unknown.
func (h hubClient) Tests(ctx context.Context, id int64) (Counts, error) {
	var v any
	if err := h.c.Get(ctx, fmt.Sprintf("/jobs/%d/tests", id), &v); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return Counts{}, nil
		}
		return Counts{}, err
	}
	return countsOf(v), nil
}

// Counts are a finished job's counts as the hub's tests route answers them;
// nil is unknown (null, absent, not a number). TestsRun is tests_run (a test
// job's PASS with an unknown count is evidence, never a requirement);
// Crates is crates_checked (a check-ni PASS that checked 0 is no verdict; an
// unknown one reads as a pass, as before the hub counted).
type Counts struct {
	TestsRun *int
	Crates   *int
}

func countsOf(v any) Counts {
	m, _ := v.(map[string]any)
	num := func(k string) *int {
		f, ok := m[k].(float64)
		if !ok {
			return nil
		}
		n := int(f)
		return &n
	}
	return Counts{TestsRun: num("tests_run"), Crates: num("crates_checked")}
}
