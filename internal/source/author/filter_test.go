package author

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

// The author-chiqun filter, as the leader's order encodes the Minister's rule.chiqun_relevance (2026-11-09).
const chiqunTitle = `(?i)^(\[(Effort router|Effort retire|Effort tagged-only|MD1 early start|MD1 subagent router|Effort dispatch[^\]]*)\]|(fix|feat|refactor|docs)\(effort\))`

var chiqunPaths = []string{
	"**/decision_judge/**", "**/effort_*", "**/history_effort*", "**/system1*",
	"aether_rust/crates/aether_routing/src/llm_handler/reasoning_effort.rs",
	"aether_rust/crates/aether_flight_defaults_kit/**/aether.reasoning_effort*",
}

func chiqunKeys() map[string]json.RawMessage {
	return map[string]json.RawMessage{model.AuthorPathsKey: model.MustJSON(chiqunPaths), model.AuthorTitleKey: model.MustJSON(chiqunTitle)}
}

func mustFilter(t *testing.T, keys map[string]json.RawMessage) *filter {
	t.Helper()
	spec, err := specFromKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	f, err := compileFilter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Titles from author-chiqun's live window (2026-11-09): the tagged series and the conventional effort scopes are
// tracked by title; the explicit skips (docs cite re-points, [quality-slices daily], [chat-ref record cites]) are not.
func TestChiqunTitles(t *testing.T) {
	f := mustFilter(t, chiqunKeys())
	for title, want := range map[string]bool{
		"[MD1 subagent router][4-15][2-3] feat(aether_services): validate the MD1 sub-agent router": true,
		"[MD1 early start][2-6][2-2] feat(reasoning-effort): start the ask at the pickup":           true,
		"[Effort retire][7-12] test(effort_prediction): retire the ledger's next-turn slot tests":   true,
		"[Effort tagged-only][3-4][1-2] refactor(effort): one composer per turn":                    true,
		"[Effort router][1-3] the router": true,
		"[Effort dispatch overlaid modelId][1-2] feat(reasoning-effort): cache_risk downgrades": true,
		"fix(effort): book the predict row from the slot's first fill":                          true,
		"docs(effort): stop saying every turn is predicted on composed history":                 true,
		"Refactor(Effort): case does not matter":                                                true,
		// The explicit skips.
		"docs(aether_runtime): re-point the leader-lease spread section's deleted record cite": false,
		"docs(runtime/config): pin the reaper and page-sync record cites at 43a2e338c7f^":      false,
		"[quality-slices daily][4-11] The reporting floor counts the sample (ADR-013)":         false,
		"[chat-ref record cites][3-5] Pin chat-ref record cites: aether_graph tool layer":      false,
		// Unrelated, and near misses.
		"[Reflection struggle score][1-5] Round tags, derived::tool_rounds":           false,
		"refactor(decision_judge): resolve the four-tier current tier once (#105316)": false, // title only; its files decide
		"chore: [Effort retire] not at the start":                                     false,
		"feat(effort_x): another scope":                                               false,
	} {
		if got := f.titleHit(title); got != want {
			t.Errorf("title %q: tracked %v, want %v", title, got, want)
		}
	}
}

func TestChiqunPaths(t *testing.T) {
	f := mustFilter(t, chiqunKeys())
	for file, want := range map[string]bool{
		"aether_rust/crates/aether_services/src/decision_judge/effort_router.rs": true,
		"aether_rust/crates/aether_services/src/decision_judge/mod.rs":           true,
		"aether_rust/crates/aether_grpc/src/effort_stamp.rs":                     true,
		"aether_rust/crates/aether_contracts_actors/src/effort_prediction.rs":    true,
		"effort_tiers.rs": true, // ** spans zero directories
		"aether_rust/crates/x/src/history_effort_ledger.rs":                                                    true,
		"aether_rust/crates/x/src/system1.rs":                                                                  true,
		"aether_rust/crates/x/src/system1/plan.rs":                                                             true, // a directory match
		"aether_rust/crates/aether_routing/src/llm_handler/reasoning_effort.rs":                                true,
		"aether_rust/crates/aether_flight_defaults_kit/flags/aether.reasoning_effort_early_start_enabled.json": true,
		"aether_rust/crates/aether_flight_defaults_kit/config_values/aether.reasoning_effort_mode.json":        true,
		// The cite re-point and quality-slices kind of change.
		"aether_rust/docs/design/2026-10-23-reasoning-effort-live.md":                   false,
		"aether_runtime/docs/configuration/reasoning-effort-prediction.md":              false,
		"aether_rust/crates/aether_flights/src/defaults.json":                           false,
		"aether_rust/crates/aether_flight_defaults_kit/flags/aether.other_enabled.json": false,
		"aether_rust/crates/aether_routing/src/llm_handler/wire_effort.rs":              false,
		"aether_rust/crates/quality_slices/src/floor.rs":                                false,
		"aether_rust/crates/x/src/my_effort_y.rs":                                       false,
	} {
		if got := f.pathHit([]string{file}); got != want {
			t.Errorf("file %s: tracked %v, want %v", file, got, want)
		}
	}
}

func TestFilterKeysAndGlobs(t *testing.T) {
	// paths as one comma-separated string (what `figaro set ... paths a,b` may store).
	f := mustFilter(t, map[string]json.RawMessage{model.AuthorPathsKey: model.MustJSON("**/a/**, b?.go")})
	if !f.pathHit([]string{"x/a/y.go"}) || !f.pathHit([]string{"b1.go"}) || f.pathHit([]string{"b12.go"}) || f.pathHit([]string{"x/b1.go"}) {
		t.Fatal("comma-separated paths misread")
	}
	if g := mustFilter(t, nil); !g.all() {
		t.Fatal("no keys must track every PR")
	}
	for _, bad := range []map[string]json.RawMessage{
		{model.AuthorTitleKey: model.MustJSON("([")},
		{model.AuthorPathsKey: model.MustJSON([]string{"src/[ab].go"})},
		{model.AuthorPathsKey: json.RawMessage(`42`)},
	} {
		spec, err := specFromKeys(bad)
		if err == nil {
			_, err = compileFilter(spec)
		}
		if err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

// files is a fake file lister counting its calls.
type fakeFiles struct {
	by    map[int][]string
	calls atomic.Int32
}

func (ff *fakeFiles) list(_ context.Context, repo string, n int) ([]string, error) {
	ff.calls.Add(1)
	fs, ok := ff.by[n]
	if !ok {
		return nil, fmt.Errorf("no PR %s#%d", repo, n)
	}
	return fs, nil
}

func authorWatch() model.Critic {
	return model.Critic{Name: "author-x", Mode: model.ModeAuthor, Source: "github-author", Host: "microsoft.ghe.com",
		FormID: "@f", Author: &model.AuthorRef{Login: "Chiqun-Zhang", Since: time.Now().Add(-time.Hour), Repos: []string{"bic/aether"}}}
}

// Through the wire: the filter keys arrive in Request.Keys; the authored key and Seen hold only tracked PRs, the
// untracked one is a number+head in the skip list; files are listed once per head; a broken filter keeps the last
// good one and says so in filter_error, which the fix removes. Fake PRs: #61586 "feat: one" (open), #61434 "feat:
// two" (merged); neither title matches, so files decide.
func TestRoundFilter(t *testing.T) {
	ff := &fakeFiles{by: map[int][]string{
		61586: {"aether_rust/crates/aether_services/src/decision_judge/effort_router.rs"},
		61434: {"aether_rust/docs/design/x.md"},
	}}
	pl := plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author"), Files: ff.list}.Handle}
	call := func(w model.Critic, keys map[string]json.RawMessage) *plugin.Reply {
		t.Helper()
		rep, err := pl.Call(context.Background(), "gh", plugin.Request{Op: plugin.OpRound, Critic: w, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Error != "" {
			t.Fatalf("round error: %s", rep.Error)
		}
		plugin.Adopt(&w, rep.Critic)
		return rep
	}
	w := authorWatch()
	// Control: without a filter both PRs are tracked and nothing is listed.
	r0 := call(w, nil)
	if a := string(r0.Set["authored"]); !strings.Contains(a, "#61586") || !strings.Contains(a, "#61434") || ff.calls.Load() != 0 {
		t.Fatalf("no filter: authored %s, %d file calls", a, ff.calls.Load())
	}

	w = authorWatch()
	keys := chiqunKeys()
	r1 := call(w, keys)
	plugin.Adopt(&w, r1.Critic)
	st := w.Runtime.Author
	if a := string(r1.Set["authored"]); !strings.Contains(a, "#61586") || strings.Contains(a, "#61434") {
		t.Fatalf("authored %s; want only #61586", a)
	}
	if len(st.Seen) != 1 || st.Skip["bic/aether#61434"] != "ce1bd297dddd" || st.Hit["bic/aether#61586"] != "ffff8f68aaaa" || ff.calls.Load() != 2 {
		t.Fatalf("state seen %v skip %v hit %v, %d file calls", st.Seen, st.Skip, st.Hit, ff.calls.Load())
	}
	if _, ok := r1.Set[model.AuthorFilterErr]; ok || len(r1.Unset) != 0 {
		t.Fatalf("a good filter wrote filter_error: set %v unset %v", r1.Set, r1.Unset)
	}

	// Same heads: the cached verdicts stand, nothing is listed, nothing is written.
	r2 := call(w, keys)
	plugin.Adopt(&w, r2.Critic)
	if ff.calls.Load() != 2 || len(r2.Set) != 0 {
		t.Fatalf("a quiet round listed files (%d calls) or wrote %v", ff.calls.Load(), r2.Set)
	}

	// A broken regex: the last good filter stays in force (same verdicts, no relisting) and filter_error says so.
	bad := chiqunKeys()
	bad[model.AuthorTitleKey] = model.MustJSON("(unclosed")
	r3 := call(w, bad)
	plugin.Adopt(&w, r3.Critic)
	if e := string(r3.Set[model.AuthorFilterErr]); !strings.Contains(e, "last good filter") || !strings.Contains(e, "title_match") {
		t.Fatalf("filter_error %q", e)
	}
	if ff.calls.Load() != 2 || r3.Set["authored"] != nil {
		t.Fatalf("a broken filter changed the verdicts: %d calls, authored %s", ff.calls.Load(), r3.Set["authored"])
	}
	// The fix removes filter_error.
	r4 := call(w, keys)
	plugin.Adopt(&w, r4.Critic)
	if strings.Join(r4.Unset, ",") != model.AuthorFilterErr || w.Runtime.Author.FilterErr != "" {
		t.Fatalf("fixed filter: unset %v err %q", r4.Unset, w.Runtime.Author.FilterErr)
	}
}

// A skipped PR whose new head touches a tracked path is news (NEW, wakes) even though it was created long ago: it
// was never shown. Control: the same PR first seen tracked in a later round with no skip history is a re-entry.
func TestPromotedPRIsNews(t *testing.T) {
	f := mustFilter(t, chiqunKeys())
	spec, _ := specFromKeys(chiqunKeys())
	prev := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	pr := forge.AuthoredPR{Repo: "bic/aether", Number: 7, Title: "docs: x", State: "OPEN", Head: "aaaa", Created: "2026-10-01T00:00:00Z"}
	ff := &fakeFiles{by: map[int][]string{7: {"docs/x.md"}}}
	st := &model.AuthorState{Seen: map[string]model.AuthorSeen{}}
	tracked, _, err := relevant(context.Background(), st, f, spec, []forge.AuthoredPR{pr}, ff.list)
	if err != nil || len(tracked) != 0 || st.Skip["bic/aether#7"] != "aaaa" {
		t.Fatalf("round 1: tracked %v skip %v err %v", tracked, st.Skip, err)
	}
	pr.Head = "bbbb"
	ff.by[7] = []string{"aether_rust/crates/s/src/decision_judge/new.rs"}
	tracked, promoted, err := relevant(context.Background(), st, f, spec, []forge.AuthoredPR{pr}, ff.list)
	if err != nil || len(tracked) != 1 || !promoted["bic/aether#7"] {
		t.Fatalf("round 2: tracked %v promoted %v err %v", tracked, promoted, err)
	}
	if got := authorKinds(authorDiff(st, tracked, prev, promoted)); got["bic/aether#7 author_pr_new"] != model.TierWake {
		t.Fatalf("promoted PR not news: %v", got)
	}
	st2 := &model.AuthorState{Seen: map[string]model.AuthorSeen{}}
	if got := authorKinds(authorDiff(st2, tracked, prev, nil)); len(got) != 0 {
		t.Fatalf("control: a re-entry was news: %v", got)
	}
}

// A file listing failure fails the round (no partial verdicts, nothing written) instead of guessing.
func TestFilesFailureFailsTheRound(t *testing.T) {
	ff := &fakeFiles{by: map[int][]string{61586: {"x"}}} // #61434 errors
	rep, err := plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author"), Files: ff.list}.Handle}.Call(context.Background(), "gh",
		plugin.Request{Op: plugin.OpRound, Critic: authorWatch(), Keys: map[string]json.RawMessage{model.AuthorPathsKey: model.MustJSON([]string{"**/y/**"})}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Error, "files of bic/aether#61434") || rep.Set["authored"] != nil || rep.Critic.Runtime.ConsecutiveFailures != 1 {
		t.Fatalf("error %q set %v failures %d", rep.Error, rep.Set, rep.Critic.Runtime.ConsecutiveFailures)
	}
}

// Live: the core reads paths and title_match off a real role form and hands them to the plugin; a broken
// title_match lands filter_error on the form.
func TestLiveFilterKeysThroughTheCore(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-author-filter")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := authorWatch()
	w.Name, w.FormID, w.CreatedAt = "livetest-authorf-"+strings.TrimPrefix(id, "@")[:8], id, time.Now().UTC()
	if err := st.SaveCritic(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, id, model.AuthorPathsKey, chiqunPaths); err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, id, model.AuthorTitleKey, chiqunTitle); err != nil {
		t.Fatal(err)
	}
	ff := &fakeFiles{by: map[int][]string{
		61586: {"aether_rust/crates/aether_grpc/src/effort_stamp.rs"},
		61434: {"aether_rust/docs/design/x.md"},
	}}
	var out strings.Builder
	p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"gh": Plugin{GH: forgetest.FakeGH(t, "author"), Files: ff.list}.Handle}}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v\n%s", err, out.String())
	}
	fm, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(fm["authored"])
	if !strings.Contains(string(raw), "#61586") || strings.Contains(string(raw), "#61434") {
		t.Fatalf("authored %s; want only #61586\n%s", raw, out.String())
	}
	if err := f.SetJSON(ctx, id, model.AuthorTitleKey, "(unclosed"); err != nil {
		t.Fatal(err)
	}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round 2: %v\n%s", err, out.String())
	}
	fm, _ = f.Form(ctx, id)
	if e, _ := fm[model.AuthorFilterErr].(string); !strings.Contains(e, "last good filter") {
		t.Fatalf("filter_error on the form: %v\n%s", fm[model.AuthorFilterErr], out.String())
	}
}
