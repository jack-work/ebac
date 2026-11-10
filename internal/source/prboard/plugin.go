package prboard

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

// Plugin serves github-prs. GH, Hub and Now replace the forge, the band hub
// and the clock in tests; nil means the real ones.
type Plugin struct {
	GH  GitHub
	Hub Hub
	Now func() time.Time
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	if req.Op != plugin.OpRound {
		return nil, fmt.Errorf("op %q not supported: board sync has no seat, no brief and no outbox", req.Op)
	}
	w := &req.Critic
	if w.BoardWatch == nil || w.BoardWatch.Author == "" || !strings.Contains(w.BoardWatch.Repo, "/") {
		return nil, fmt.Errorf("%s: board sync without an author and an owner/repo", w.Name)
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
	if len(w.Runtime.PRBoard) > 0 {
		if err := json.Unmarshal(w.Runtime.PRBoard, &prev); err != nil {
			prev = State{}
		}
	}
	rep := &plugin.Reply{Critic: w, Done: true}
	r, err := Run(ctx, gh, hub, prev, Pass{Watch: *w.BoardWatch, Inputs: req.Inputs, Board: req.Board, Children: req.Children, Notes: req.Notes, Now: now()})
	for _, l := range r.Log {
		rep.Logf("%s: %s", w.Name, l)
	}
	if err != nil {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep, nil
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = now().UTC()
	w.Runtime.PRBoard = model.MustJSON(r.State)
	rep.Forms, rep.Pings = r.Forms, r.Pings
	rep.Logf("%s: round %d: %d GitHub call(s), %d PR form(s) written, %d tracked", w.Name, w.Runtime.Round, r.GHCalls, len(r.Changed), len(r.State.PRs))
	return rep, nil
}

// hubClient reads the band hub as a metadata reader.
type hubClient struct{ c *bandstate.Client }

func (h hubClient) Jobs(ctx context.Context, since time.Time) ([]HubJob, error) {
	var jobs []HubJob
	q := url.Values{"since": {fmt.Sprint(since.UnixMilli())}, "sort": {"updated"}, "order": {"desc"}, "limit": {"2000"}}
	return jobs, h.c.Get(ctx, "/jobs?"+q.Encode(), &jobs)
}

// Tests reads GET /jobs/<id>/tests: a number, or an object carrying it under tests. Until the route exists (404)
// the count is unknown.
func (h hubClient) Tests(ctx context.Context, id int64) (int, bool, error) {
	var v any
	if err := h.c.Get(ctx, fmt.Sprintf("/jobs/%d/tests", id), &v); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return 0, false, nil
		}
		return 0, false, err
	}
	switch x := v.(type) {
	case float64:
		return int(x), true, nil
	case map[string]any:
		for _, k := range []string{"tests_run", "tests", "run", "total"} {
			if f, ok := x[k].(float64); ok {
				return int(f), true, nil
			}
		}
	}
	return 0, false, nil
}
