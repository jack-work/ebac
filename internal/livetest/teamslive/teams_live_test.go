package teamslive

// EBAC_LIVE tests of the teams plugin: they need core's form store, poller and
// the live daemon, so they live in core and drive the plugin binary by exec
// (EBAC_PLUGIN_DIR names the directory holding ebac-tomb; unset -> skip).
// tomb is faked by testdata/fake-tomb (EBAC_TOMB), band-cred by a one-line
// script (EBAC_BAND_CRED); FAKE_SCENARIO picks the chat a round sees.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/livetest"
	"github.com/gluck/ebac/internal/pluginrun"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func liveOrSkip(t *testing.T) *figaro.Figaro {
	t.Helper()
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	if os.Getenv("EBAC_PLUGIN_DIR") == "" {
		t.Skip("EBAC_PLUGIN_DIR unset: build ebac-tomb (go build -C plugins/teams -o <dir> ./cmd/ebac-tomb) and point it there")
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// teamsGoneAfter is the plugin's consecutive-gone threshold, written literally
// (a changed threshold in plugins/teams must turn this red).
const teamsGoneAfter = 3

func teamsTestChat() *model.ChatRef {
	return &model.ChatRef{ID: "19:abc@thread.v2", Topic: "MD1 + Cowork"}
}

// teamsPlugin runs the real ebac-tomb binary for one round against the fake
// tomb scenario.
func teamsPlugin(t *testing.T, scenario string) plugin.Caller {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	t.Setenv("EBAC_TOMB", filepath.Join(filepath.Dir(here), "testdata", "fake-tomb"))
	cred := filepath.Join(t.TempDir(), "band-cred")
	if err := os.WriteFile(cred, []byte("#!/bin/sh\n[ \"$1\" = token ] && echo faketoken\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EBAC_BAND_CRED", cred)
	t.Setenv("EBAC_BAND_CRED_SOCKET", "")
	t.Setenv("FAKE_SCENARIO", scenario)
	t.Setenv(pluginrun.SandboxOffEnv, "off")
	r, err := pluginrun.NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLiveTeamsRoundsBaselineNewsAndGone(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-teams")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, id)
	w := &model.Critic{Name: "livetest-teams-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: teamsTestChat()}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) error {
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: teamsPlugin(t, scenario)}
		return p.Poll(ctx, w.Name)
	}
	load := func() *model.Critic {
		c, err := st.LoadCritic(ctx, w.Name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if err := round("ok"); err != nil {
		t.Fatalf("baseline round: %v", err)
	}
	if c := load(); len(c.Runtime.Teams.Seen) != 1 || len(c.Runtime.PendingEvents) != 0 {
		t.Fatalf("baseline: seen %v pending %v", c.Runtime.Teams.Seen, c.Runtime.PendingEvents)
	}

	// News with nobody seated: recorded, inbox written, delivery refused loudly.
	if err := round("ok2"); err != nil {
		t.Fatalf("news round: %v", err)
	}
	c := load()
	if len(c.Runtime.PendingEvents) != 1 || !strings.Contains(c.Runtime.LastError, "no reachable target-aria") {
		t.Fatalf("news: pending %+v lastError %q", c.Runtime.PendingEvents, c.Runtime.LastError)
	}
	form, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	inbox, _ := json.Marshal(form["inbox"])
	if !strings.Contains(string(inbox), "please look") {
		t.Fatalf("inbox lacks the new message: %s", inbox)
	}

	// Blind: a failure younger than teamsBlindAfter writes nothing (control) ...
	hasBlind := func() bool {
		form, err := f.Form(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := form["blind"]
		return ok
	}
	_ = round("down")
	if hasBlind() || load().Runtime.Teams.Blind {
		t.Fatal("one failed round already marked the chat blind")
	}
	// ... one past it marks the form and stays marked ...
	t.Setenv("EBAC_TEAMS_BLIND_AFTER", "0s") // the plugin is another process: shorten its threshold by env
	_ = round("down")
	if c := load(); !hasBlind() || !c.Runtime.Teams.Blind || c.Runtime.Teams.FailSince == "" {
		t.Fatalf("not blind after the threshold: %+v", c.Runtime.Teams)
	}
	_ = round("down")
	if !hasBlind() {
		t.Fatal("blind marker vanished while still failing")
	}
	// ... and the first good round clears it.
	if err := round("ok2"); err != nil {
		t.Fatalf("recovery round: %v", err)
	}
	if c := load(); hasBlind() || c.Runtime.Teams.Blind || c.Runtime.Teams.FailSince != "" {
		t.Fatalf("blind not cleared on recovery: %+v", c.Runtime.Teams)
	}
	t.Setenv("EBAC_TEAMS_BLIND_AFTER", "")

	// Transient failures never count toward gone; two gones then a success reset the streak.
	_ = round("auth403")
	_ = round("gone404")
	_ = round("gone404")
	if c := load(); c.Runtime.Teams.GoneStreak != 2 || c.Runtime.Stopped {
		t.Fatalf("after auth403+2x404: streak %d stopped %v", c.Runtime.Teams.GoneStreak, c.Runtime.Stopped)
	}
	_ = round("ok2")
	if c := load(); c.Runtime.Teams.GoneStreak != 0 {
		t.Fatalf("success did not reset the streak: %d", c.Runtime.Teams.GoneStreak)
	}
	for i := 0; i < teamsGoneAfter; i++ {
		_ = round("gone404")
	}
	c = load()
	if !c.Runtime.Stopped || !strings.Contains(c.Runtime.StopReason, "chat gone") {
		t.Fatalf("not stopped after %d gones: %+v", teamsGoneAfter, c.Runtime)
	}
	form, _ = f.Form(ctx, id)
	gone, _ := json.Marshal(form["gone"])
	if !strings.Contains(string(gone), "19:abc@thread.v2") {
		t.Fatalf("gone marker missing: %s", gone)
	}
	if err := round("ok2"); err != nil || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("a stopped critic polled again: %v\n%s", err, out.String())
	}
}

// One teams critic through: a legacy quiet list migrated to rules on its first round; a migrated quiet sender
// recorded, not woken; a broken rule set that keeps the last good set and says so in trigger_error; and a fixed set
// that clears the error and is in force on the same round.
func TestLiveTeamsTriggerMigrationAndFallback(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-trigger")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, id)
	w := &model.Critic{Name: "livetest-trigger-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: teamsTestChat()}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	// What a pre-trigger binary left on the form: chat.quiet in the critic config.
	if err := st.Commit(ctx, id, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		var cfg map[string]any
		_ = json.Unmarshal(cur["critic"], &cfg)
		cfg["chat"].(map[string]any)["quiet"] = []string{"Jack Kelliher", "Akshay"}
		return map[string]json.RawMessage{"critic": model.MustJSON(cfg)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) {
		t.Helper()
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: teamsPlugin(t, scenario)}
		if err := p.Poll(ctx, w.Name); err != nil {
			t.Fatalf("round %s: %v\n%s", scenario, err, out.String())
		}
	}
	keys := func() map[string]json.RawMessage {
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return cf.Keys
	}
	load := func() *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, w.Name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	round("ok") // baseline + migration
	k := keys()
	if !strings.Contains(string(k["trigger"]), "Akshay") || strings.Contains(string(k["trigger"]), "Jack") {
		t.Fatalf("migrated trigger = %s; want Akshay recorded and the operator name dropped", k["trigger"])
	}
	if strings.Contains(string(k["critic"]), "quiet") {
		t.Fatalf("legacy quiet still in the critic config: %s", k["critic"])
	}

	// Akshay is a migrated quiet sender: recorded, so nothing pending and no seat needed.
	round("ok2")
	if c := load(); len(c.Runtime.PendingEvents) != 0 || c.Runtime.LastError != "" {
		t.Fatalf("migrated quiet sender woke: pending %+v err %q", c.Runtime.PendingEvents, c.Runtime.LastError)
	}

	// A broken set: the last good one (Akshay recorded) stays in force, and the form says why.
	if err := f.SetJSON(ctx, id, "trigger", json.RawMessage(`[{"from":"x","action":"wake"}]`)); err != nil {
		t.Fatal(err)
	}
	round("ok3")
	k = keys()
	if c := load(); len(c.Runtime.PendingEvents) != 0 {
		t.Fatalf("broken set: Akshay woke, so the last good set was not kept: %+v", c.Runtime.PendingEvents)
	}
	if !strings.Contains(string(k["trigger_error"]), "last good set") || !strings.Contains(string(k["trigger_error"]), "last rule must be") {
		t.Fatalf("trigger_error = %s", k["trigger_error"])
	}

	// Fixed: in force on this round (Akshay wakes; nobody is seated, so the wake is refused loudly), error removed.
	if err := f.SetJSON(ctx, id, "trigger", json.RawMessage(`[{"default":true,"action":"wake"}]`)); err != nil {
		t.Fatal(err)
	}
	round("ok4")
	k = keys()
	if _, ok := k["trigger_error"]; ok {
		t.Fatalf("trigger_error survived a good set: %s", k["trigger_error"])
	}
	if c := load(); len(c.Runtime.PendingEvents) != 1 || !strings.Contains(c.Runtime.LastError, "no reachable target-aria") {
		t.Fatalf("fixed set not in force: pending %+v err %q", c.Runtime.PendingEvents, c.Runtime.LastError)
	}
}

// Through the core on a real role form: in collect mode a message lands in the inbox at once while the ping waits
// for its tick (no delivery attempt, so no error even with no seat); control: without a ping cadence the same news
// is delivered at once (attempted: no seat, so the empty-seat error).
func TestLiveTeamsCollectModeCollectsWithoutPinging(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		ping    int
		attempt bool
	}{{"collect", 1800, false}, {"control", 0, true}} {
		st, err := form.NewStore(f)
		if err != nil {
			t.Fatal(err)
		}
		st.Root = t.TempDir()
		id, err := f.FormNew(ctx, "ebac-livetest-collect")
		if err != nil {
			t.Fatal(err)
		}
		livetest.Remove(t, f, id)
		w := &model.Critic{Name: "livetest-" + c.name + "-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
			CreatedAt: time.Now().UTC(), FormID: id, Chat: teamsTestChat(), PingEverySec: c.ping}
		w.Runtime.LastPing = time.Now().UTC() // a ping just went out: the next is 30 minutes away
		if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
			t.Fatal(err)
		}
		round := func(scenario string) {
			t.Helper()
			var out strings.Builder
			p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: teamsPlugin(t, scenario)}
			if err := p.Poll(ctx, w.Name); err != nil {
				t.Fatalf("%s %s: %v\n%s", c.name, scenario, err, out.String())
			}
		}
		round("ok")
		round("ok2")
		fm, err := f.Form(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		inbox, _ := json.Marshal(fm["inbox"])
		if !strings.Contains(string(inbox), "please look") {
			t.Fatalf("%s: the message is not in the inbox: %s", c.name, inbox)
		}
		got, _ := st.LoadCritic(ctx, w.Name)
		attempted := strings.Contains(got.Runtime.LastError, "no reachable target-aria")
		if attempted != c.attempt || len(got.Runtime.PendingEvents) != 1 {
			t.Fatalf("%s: delivery attempted %v (want %v), pending %d, error %q", c.name, attempted, c.attempt, len(got.Runtime.PendingEvents), got.Runtime.LastError)
		}
	}
}

// leaves flattens a form into path -> JSON value, skipping system (it never
// reaches a studier).
func leaves(prefix string, v any, out map[string]string) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for k, x := range m {
			if prefix == "" && k == "system" {
				continue
			}
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			leaves(p, x, out)
		}
		return
	}
	b, _ := json.Marshal(v)
	out[prefix] = string(b)
}

// Live, on the daemon: a Teams round writes only what is new. An unchanged
// round changes no studier-visible leaf (inbox_fetch included); a round with
// one new message changes only that message's leaves (control).
func TestLiveTeamsRoundWritesOnlyNewLeaves(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-teams-leaves")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	livetest.Remove(t, f, id)
	w := &model.Critic{Name: "livetest-leaves-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: teamsTestChat()}
	if err := st.SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) {
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: teamsPlugin(t, scenario)}
		if err := p.Poll(ctx, w.Name); err != nil {
			t.Fatalf("round %s: %v", scenario, err)
		}
	}
	snap := func() map[string]string {
		form, err := f.Form(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		leaves("", map[string]any(form), m)
		return m
	}
	changed := func(a, b map[string]string) []string {
		var c []string
		for k, v := range b {
			if a[k] != v {
				c = append(c, k)
			}
		}
		for k := range a {
			if _, ok := b[k]; !ok {
				c = append(c, "-"+k)
			}
		}
		sort.Strings(c)
		return c
	}
	round("ok")
	round("ok2")
	before := snap()
	round("ok2") // nothing new
	same := snap()
	if c := changed(before, same); len(c) != 0 {
		t.Fatalf("an unchanged round changed studier leaves: %v", c)
	}
	round("ok3") // message 3 is new
	after := snap()
	c := changed(same, after)
	t.Logf("leaves written by a round with one new message: %v", c)
	if len(c) == 0 {
		t.Fatal("control: a new message wrote nothing")
	}
	for _, p := range c {
		if !strings.HasPrefix(p, "inbox.teams-3.") {
			t.Fatalf("a round with one new message touched %q (all: %v)", p, c)
		}
	}
}

// The 2026-11-10 wipe: after a TLS outage the blind-recovery round of 4 Teams critics wrote critic: {} and they
// vanished from ebac ls. Replays blind then recover over the real merge path, a FRESH store per round (each round is
// its own process in production), and asserts the critic key is byte-identical afterwards.
func TestLiveBlindRecoveryKeepsTheCritic(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-wipe")
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, f, id)
	root := t.TempDir()
	store := func() *form.Store {
		st, err := form.NewStore(f)
		if err != nil {
			t.Fatal(err)
		}
		st.Root = root
		return st
	}
	w := &model.Critic{Name: "livetest-wipe-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, EverySec: 60, Chat: teamsTestChat()}
	if err := store().SaveCriticRecordingForTest(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := store().SeedTrigger(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) error {
		p := &poll.Poller{Store: store(), Fig: f, Out: &out, Plugins: teamsPlugin(t, scenario)}
		return p.Poll(ctx, w.Name)
	}
	critic := func() string {
		keys, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(keys["critic"])
	}
	if err := round("ok"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// The victim forms' top-level key set (teams-akshay-chiqun-minister @d7664712): the tree shape the old form
	// library mis-deleted depends on it. target-aria names no live aria, so nothing is ever woken.
	for k, v := range map[string]any{"ack_at": "2026-11-10T18:00:00Z", "ack_note": "n", "ack_round": 1, "inbox": map[string]any{},
		"target-aria": "0000000000000000deadbeef00000000"} {
		if err := f.SetJSON(ctx, id, k, v); err != nil {
			t.Fatal(err)
		}
	}
	before := critic()
	t.Setenv("EBAC_TEAMS_BLIND_AFTER", "0s") // the plugin is another process: shorten its threshold by env
	for i := 0; i < 3; i++ {
		_ = round("down")
	}
	if err := round("ok"); err != nil {
		t.Fatalf("recovery: %v\n%s", err, out.String())
	}
	if after := critic(); after != before {
		t.Fatalf("critic changed across blind + recovery:\n before %s\n after  %s\n%s", before, after, out.String())
	}
	keys, _, _ := f.FormRaw(ctx, id)
	if _, blind := keys["blind"]; blind {
		t.Fatalf("recovery left blind on the form\n%s", out.String())
	}
	if _, ok := keys["trigger"]; !ok {
		t.Fatal("trigger lost")
	}
	if _, err := store().LoadCritic(ctx, w.Name); err != nil {
		t.Fatalf("critic unreadable after recovery: %v", err)
	}
}
