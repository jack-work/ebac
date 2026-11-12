package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// ghCLI answers GraphQL through the gh CLI as the calling user (read-only queries only), and records the
// mergeability-only queries.
type ghCLI struct {
	host  string
	light []string
}

func (g *ghCLI) GraphQL(ctx context.Context, q string) (json.RawMessage, error) {
	if strings.Contains(q, "{number mergeable mergeStateStatus}") {
		g.light = append(g.light, q)
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+q)
	cmd.Env = append(os.Environ(), "GH_HOST="+g.host)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh: %v: %.300s", err, out)
	}
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// Live and read-only (EBAC_LIVE_GH=1, snapshots in EBAC_LIVE_DIR: live-form.json, live-inputs.json,
// live-board.json from figaro state -j): this code's passes over the live scout state against live GitHub. Nothing
// is written anywhere; it logs what each pass would do. The last pass stages a conflict in memory: every tracked
// open PR GitHub now reports CONFLICTING is set back to last-known MERGEABLE and its base head forgotten, so the
// watch must find each, re-read it, and ping its owner.
func TestLiveBaseWatchReadOnly(t *testing.T) {
	if os.Getenv("EBAC_LIVE_GH") != "1" {
		t.Skip("set EBAC_LIVE_GH=1 and EBAC_LIVE_DIR to run read-only against live GitHub")
	}
	dir := os.Getenv("EBAC_LIVE_DIR")
	load := func(name string) map[string]json.RawMessage {
		b, err := os.ReadFile(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	form, inputs, board := load("live-form.json"), load("live-inputs.json"), load("live-board.json")
	var sys struct {
		Ebac struct {
			Runtime struct {
				Scout State `json:"scout"`
			} `json:"runtime"`
		} `json:"ebac"`
	}
	if err := json.Unmarshal(form["system"], &sys); err != nil {
		t.Fatal(err)
	}
	var critic struct {
		Watch model.ScoutWatch `json:"scout_watch"`
	}
	if err := json.Unmarshal(form["critic"], &critic); err != nil {
		t.Fatal(err)
	}
	st := sys.Ebac.Runtime.Scout
	gh := &ghCLI{host: "microsoft.ghe.com"}
	now := time.Now().UTC()
	pass := func(label string) Result {
		t.Helper()
		gh.light = nil
		r, err := Run(context.Background(), gh, nil, st, Pass{Watch: critic.Watch, Inputs: inputs, Board: board, Now: now})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		st = r.State
		open := 0
		for _, p := range st.PRs {
			if p.State == "OPEN" {
				open++
			}
		}
		t.Logf("%s: %d GitHub call(s), %d mergeability quer(y/ies), %d entries set, %d open PRs, %d bases, recheck %v, pings %d",
			label, r.GHCalls, len(gh.light), len(r.Set), open, len(st.Bases), st.Recheck, len(r.Pings))
		for _, l := range r.Log {
			if strings.Contains(l, "base") || strings.Contains(l, "conflict") || strings.Contains(l, "mergeab") {
				t.Logf("  log: %s", l)
			}
		}
		for _, p := range r.Pings {
			t.Logf("  ping %s: %s", p.To, p.Text)
		}
		now = now.Add(2 * time.Minute)
		return r
	}
	pass("A schema reshape")
	pass("B first sighting of the bases")
	r := pass("C quiet")
	if len(gh.light) != 0 {
		t.Errorf("quiet pass read mergeability")
	}
	_ = r

	// Stage: what GitHub says now, by the same light query.
	var open []int
	for n, p := range st.PRs {
		if p.State == "OPEN" {
			open = append(open, n)
		}
	}
	sort.Ints(open)
	var conflicting []int
	for i := 0; i < len(open); i += LightPage {
		data, err := gh.GraphQL(context.Background(), mergeQuery(critic.Watch.Repo, open[i:min(i+LightPage, len(open))]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeMerge(data)
		if err != nil {
			t.Fatal(err)
		}
		for n, m := range got {
			if settledConflict(m.Mergeable, m.MergeStateStatus) {
				conflicting = append(conflicting, n)
			}
		}
	}
	sort.Ints(conflicting)
	t.Logf("GitHub now: %d of %d tracked open PRs conflicting: %v", len(conflicting), len(open), conflicting)
	if len(conflicting) == 0 {
		t.Skip("no live conflict to stage")
	}
	for _, n := range conflicting {
		st.PRs[n].Mergeable = "MERGEABLE"
		delete(st.Bases, baseOf(st.PRs[n]))
	}
	r = pass("D staged: conflicting PRs last known mergeable, their bases moved")
	for _, n := range conflicting {
		if !strings.Contains(fmt.Sprint(r.Pings), fmt.Sprintf("#%d ", n)) && st.PRs[n].Owner > 0 {
			t.Errorf("#%d (owner rule %d) not pinged", n, st.PRs[n].Owner)
		}
		if st.PRs[n].Mergeable != "CONFLICTING" {
			t.Errorf("#%d not re-read: last known %q", n, st.PRs[n].Mergeable)
		}
	}
	pass("E quiet after")
}
