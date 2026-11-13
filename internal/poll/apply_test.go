package poll

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/rosterstate"
)

// One round through a fake plugin against a live form: the core applies the
// trigger rules to the plugin's events, drops a dropped event's inbox item
// with it, keeps the plugin away from core-owned fields, and counts a plugin
// failure as a failed round.
func TestLiveCoreAppliesAPluginReply(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-apply")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-apply-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: &model.ChatRef{ID: "19:x", Topic: "t"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	rules := `[{"from":"spam","action":"drop"},{"contains":"urgent","action":"wake"},{"default":true,"action":"record"}]`
	if err := st.Commit(ctx, id, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{"trigger": json.RawMessage(rules)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	var fail error
	fake := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		if fail != nil {
			return nil, fail
		}
		c := req.Critic
		c.Write, c.FormID = true, "@hijack" // core-owned: must not stick
		ev := func(id, who, text string) model.Event {
			e := model.Event{Kind: "teams_new", Tier: model.TierWake, PR: "t", Comment: id, Author: who, Detail: "d"}
			e.Text = text
			return e
		}
		rep := &plugin.Reply{Critic: &c, Events: plugin.Wire([]model.Event{
			ev("1", "spam", "buy now"), ev("2", "friend", "this is URGENT"), ev("3", "friend", "fyi")}),
			Inbox: map[string]form.InboxItem{}}
		for i, e := range rep.Events {
			k := "x-" + e.Comment
			rep.Inbox[k] = form.InboxItem{Kind: "teams_new", By: e.Author, At: "2026-11-09T00:00:00Z", Body: e.Text}
			rep.Events[i].Inbox = []string{k}
		}
		rep.SetKey("blind", map[string]int{"n": 3})
		return rep, nil
	}
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"tomb": fake}}

	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v\n%s", err, out.String())
	}
	got, err := st.LoadCritic(ctx, w.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Write || got.FormID != id {
		t.Fatalf("the plugin rewrote a core field: write=%v form=%s", got.Write, got.FormID)
	}
	tiers := map[string]model.Tier{}
	for _, e := range got.Runtime.PendingEvents {
		tiers[e.Comment] = e.Tier
	}
	if _, ok := tiers["1"]; ok {
		t.Fatalf("a dropped event is pending: %+v", got.Runtime.PendingEvents)
	}
	if tiers["2"] != model.TierWake || tiers["3"] != model.TierRecord {
		t.Fatalf("rules not applied (contains matches the wire text): %+v", tiers)
	}
	fm, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	inbox := model.ObjectOf(model.MustJSON(fm["inbox"]))
	if _, ok := inbox["x-1"]; ok {
		t.Fatalf("a dropped event's inbox item reached the form: %v", keys(inbox))
	}
	if _, ok := inbox["x-2"]; !ok {
		t.Fatalf("a kept event's inbox item is missing: %v", keys(inbox))
	}
	if fm["blind"] == nil {
		t.Fatalf("the plugin's form key was not written: %v", fm)
	}

	// A plugin that fails outright is a failed round, with its error.
	fail = errors.New("plugin exploded")
	if err := p.Poll(ctx, w.Name); err == nil || !strings.Contains(err.Error(), "plugin exploded") {
		t.Fatalf("err = %v", err)
	}
	got, _ = st.LoadCritic(ctx, w.Name)
	if got.Runtime.ConsecutiveFailures != 1 || !strings.Contains(got.Runtime.LastError, "plugin exploded") {
		t.Fatalf("failure not counted: %d %q", got.Runtime.ConsecutiveFailures, got.Runtime.LastError)
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A plugin writes only source keys. Positive control first: threads is
// applied; then a reply naming target-aria is refused whole, with the key and
// the plugin in the error, and changes nothing on the form.
func TestSourceKeysOnly(t *testing.T) {
	ok := &plugin.Reply{}
	ok.SetKey("threads", map[string]int{"n": 1})
	ok.Unset = []string{"blind"}
	if err := SourceKeysOnly("keel-threads", ok); err != nil {
		t.Fatalf("a source key was refused: %v", err)
	}
	for _, c := range []struct {
		rep  *plugin.Reply
		want string
	}{
		{&plugin.Reply{Set: map[string]json.RawMessage{"target-aria": json.RawMessage(`"x"`)}}, `plugin keel may not set form key "target-aria"`},
		{&plugin.Reply{Set: map[string]json.RawMessage{"trigger": json.RawMessage(`[]`)}}, `may not set form key "trigger"`},
		{&plugin.Reply{Unset: []string{"succession.mode"}}, `may not unset form key "succession.mode"`},
	} {
		err := SourceKeysOnly("keel-threads", c.rep)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("err = %v, want %q", err, c.want)
		}
	}

	// Per source: each owns its keys and no other source's.
	hub := &plugin.Reply{}
	for _, k := range []string{"queues", "nodes", "live", "dupes", "updated"} {
		hub.SetKey(k, []string{})
	}
	if err := SourceKeysOnly(model.SourceBandHub, hub); err != nil {
		t.Fatalf("band-hub's own keys refused: %v", err)
	}
	// The roster's role:<name> pattern: one more segment, no dots or stars.
	ros := &plugin.Reply{}
	for _, k := range []string{"role:cheroot-boss", "role:loner%2Ev2", "forks", "updated"} {
		ros.SetKey(k, "x")
	}
	ros.Unset = []string{"role:gone"}
	if err := SourceKeysOnly(model.SourceRoster, ros); err != nil {
		t.Fatalf("the roster's own keys refused: %v", err)
	}
	for _, c := range []struct{ source, key, want string }{
		{model.SourceRoster, "target-aria", `plugin roster may not set form key "target-aria"`},
		{model.SourceRoster, "role:", `plugin roster may not set form key "role:"`},
		{model.SourceRoster, "role:a.target-aria", `plugin roster may not set form key "role:a.target-aria"`},
		{model.SourceRoster, "role:*", `plugin roster may not set form key "role:*"`},
		{model.SourceRoster, "queues", `plugin roster may not set form key "queues"`},
		{"teams-chat", "threads", `plugin tomb may not set form key "threads" (teams-chat keys: blind, inbox_fetch)`},
		{"", "authored", `plugin gh may not set form key "authored" (github-pr keys: none)`},
		{model.SourceBandHub, "blind", `plugin bandhub may not set form key "blind"`},
		{"github-author", "queues", `plugin gh may not set form key "queues"`},
	} {
		rep := &plugin.Reply{}
		rep.SetKey(c.key, 1)
		if err := SourceKeysOnly(c.source, rep); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s setting %s: err = %v, want %q", c.source, c.key, err, c.want)
		}
	}
}

func TestLiveAPluginCannotWriteTheSeat(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-keys")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-keys-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeKeel, Source: "keel-threads",
		CreatedAt: time.Now().UTC(), FormID: id, Keel: &model.KeelRef{Repo: "notes/x"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	hijack := false
	fake := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		c := req.Critic
		rep := &plugin.Reply{Critic: &c}
		rep.SetKey("threads", map[string]int{"round": c.Runtime.Round})
		if hijack {
			rep.SetKey("target-aria", "ffffffffffffffffffffffffffffffff")
		}
		return rep, nil
	}
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"keel": fake}}

	// Positive control: a source key lands.
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v", err)
	}
	fm, _ := f.Form(ctx, id)
	if th, _ := fm["threads"].(map[string]any); th == nil || th["round"] != float64(1) {
		t.Fatalf("control: threads not applied: %v", fm["threads"])
	}

	hijack = true
	err = p.Poll(ctx, w.Name)
	if err == nil || !strings.Contains(err.Error(), `plugin keel may not set form key "target-aria"`) {
		t.Fatalf("err = %v", err)
	}
	t.Logf("refused: %v", err)
	fm, _ = f.Form(ctx, id)
	if fm["target-aria"] != nil {
		t.Fatalf("the plugin seated an aria: %v", fm["target-aria"])
	}
	if th, _ := fm["threads"].(map[string]any); th["round"] != float64(1) {
		t.Fatalf("a refused reply was partly applied: threads=%v", fm["threads"])
	}
	got, _ := st.LoadCritic(ctx, w.Name)
	if got.Runtime.ConsecutiveFailures != 1 || !strings.Contains(got.Runtime.LastError, "target-aria") {
		t.Fatalf("refusal not counted as a failed round: %d %q", got.Runtime.ConsecutiveFailures, got.Runtime.LastError)
	}
}

// The core hands the roster plugin the real aria store: a round writes a
// role:<name> key for every role and the forks list; a second round on the
// same store writes no roster key.
func TestLiveRosterRoundReadsTheStore(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-roster")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-roster-" + strings.TrimPrefix(id, "@")[:8], Source: model.SourceRoster,
		CreatedAt: time.Now().UTC(), FormID: id}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	var seen int
	var lastSet []string
	fixed := time.Now()
	h := rosterstate.Plugin{Now: func() time.Time { return fixed }}.Handle
	spy := func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		seen = len(req.Arias)
		rep, err := h(ctx, req)
		lastSet = nil
		if rep != nil {
			for k := range rep.Set {
				lastSet = append(lastSet, k)
			}
		}
		return rep, err
	}
	var out strings.Builder
	p := &Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"roster": spy}}
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round: %v\n%s", err, out.String())
	}
	if seen == 0 {
		t.Fatal("the core passed the roster no arias")
	}
	fm, _ := f.Form(ctx, id)
	roles := 0
	for k := range fm {
		if strings.HasPrefix(k, "role:") {
			roles++
		}
	}
	if roles == 0 || fm["forks"] == nil {
		t.Fatalf("roster keys missing: %d role keys, forks %v", roles, fm["forks"] != nil)
	}
	// updated was dropped (118ac63): the roster writes only on change, so a
	// timestamp would rewrite the form every round. It must stay gone.
	if fm["updated"] != nil {
		t.Fatalf("roster wrote the dropped key updated: %s", fm["updated"])
	}
	t.Logf("store: %d arias, %d roles on the form", seen, roles)

	// A second round soon after: the store may move (other seats are live),
	// so the control is that it writes only what changed, never all of it.
	if err := p.Poll(ctx, w.Name); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(lastSet) >= roles {
		t.Fatalf("round 2 rewrote %d keys of %d: not change-only", len(lastSet), roles)
	}
	t.Logf("round 2 wrote %v", lastSet)
}
