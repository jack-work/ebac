package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/tomb"
	"github.com/gluck/ebac/internal/trigger"
)

func tmsg(id, from, content string) tomb.TeamsMsg {
	return tomb.TeamsMsg{ID: id, Version: id, ComposeTime: "2026-10-21T22:00:00Z", MessageType: "RichText/Html", From: from, Content: content}
}

func testChat() *model.ChatRef {
	return &model.ChatRef{ID: "19:abc@thread.v2", Topic: "MD1 + Cowork"}
}

func baseline(t *testing.T, msgs ...tomb.TeamsMsg) *model.TeamsState {
	t.Helper()
	st := &model.TeamsState{}
	ev, items := teamsDiff(st, msgs, testChat())
	if len(ev) != 0 || len(items) != 0 {
		t.Fatalf("first round announced %d event(s), %d item(s); it must only record", len(ev), len(items))
	}
	return st
}

func TestTeamsFirstRoundBaselinesSilently(t *testing.T) {
	st := baseline(t, tmsg("100", "Akshay", "<p>hi</p>"), tmsg("101", "Liang", "yo"),
		tomb.TeamsMsg{ID: "102", MessageType: "ThreadActivity/AddMember"})
	if len(st.Seen) != 2 {
		t.Fatalf("seen = %v; want the two messages and no thread activity", st.Seen)
	}
	// Positive control: the same state now reports a new message.
	ev, _ := teamsDiff(st, []tomb.TeamsMsg{tmsg("103", "Akshay", "new")}, testChat())
	if len(ev) != 1 {
		t.Fatalf("after the baseline a new message gave %d events", len(ev))
	}
}

func TestTeamsNewMessageWakesWithInboxItem(t *testing.T) {
	st := baseline(t, tmsg("100", "Akshay", "old"))
	ev, items := teamsDiff(st, []tomb.TeamsMsg{tmsg("100", "Akshay", "old"),
		tmsg("200", "Liang Shi", `<p>Ship it &amp; tell <b>Jack</b><br>line two</p><img src="x">`)}, testChat())
	if len(ev) != 1 || ev[0].Kind != "teams_new" || ev[0].Tier != model.TierWake || ev[0].Author != "Liang Shi" {
		t.Fatalf("events = %+v", ev)
	}
	it, ok := items["teams-200"]
	if !ok {
		t.Fatalf("no inbox item for 200: %v", items)
	}
	if it.Body != "Ship it & tell Jack\nline two" {
		t.Fatalf("body = %q", it.Body)
	}
	// chat and fetch live once in inbox_fetch, not on the item.
	if !it.HasImage || it.Fetch != "" || it.Chat != "" || it.By != "Liang Shi" {
		t.Fatalf("item = %+v", it)
	}
	f := InboxFetch("19:abc@thread.v2")
	if f["chat"] != "19:abc@thread.v2" || !strings.Contains(f["images"], "--download-images") || !strings.Contains(f["text"], `select(.id=="<id>")`) {
		t.Fatalf("inbox_fetch = %v", f)
	}
}

// teamsRules is what the core does to a teams round: the default set, or a
// migrated legacy quiet list.
func teamsRules(t *testing.T, ev []model.Event, rules trigger.Rules) map[string]model.Event {
	t.Helper()
	kept, dropped := rules.Apply(ev)
	if len(dropped) != 0 {
		t.Fatalf("teams defaults dropped %+v", dropped)
	}
	out := map[string]model.Event{}
	for _, e := range kept {
		out[e.Comment] = e
	}
	return out
}

// A legacy quiet sender, migrated, is recorded (any case) and still reaches the inbox.
func TestTeamsMigratedQuietSenderIsRecordedNotWoken(t *testing.T) {
	st := baseline(t)
	ev, items := teamsDiff(st, []tomb.TeamsMsg{tmsg("300", "liang shi", "note"), tmsg("301", "Akshay", "control")}, testChat())
	by := teamsRules(t, ev, trigger.MigrateQuiet([]string{"Liang Shi", "Jack Kelliher"}, "Jack Kelliher"))
	if by["300"].Tier != model.TierRecord {
		t.Fatalf("migrated quiet sender (case-insensitive) must be TierRecord: %+v", by["300"])
	}
	if by["301"].Tier != model.TierWake {
		t.Fatalf("control: a sender not in the quiet list must wake: %+v", by["301"])
	}
	if _, ok := items["teams-300"]; !ok {
		t.Fatal("quiet sender's message must still reach the inbox")
	}
}

func TestTeamsEditIsContentNotVersion(t *testing.T) {
	st := baseline(t, tmsg("400", "Akshay", "v1"))
	// A reaction bumps version but not content: no event.
	r := tmsg("400", "Akshay", "v1")
	r.Version = "999"
	r.Properties = map[string]any{"emotions": []any{map[string]any{"key": "like"}}}
	if ev, _ := teamsDiff(st, []tomb.TeamsMsg{r}, testChat()); len(ev) != 0 {
		t.Fatalf("a reaction produced %+v", ev)
	}
	ev, items := teamsDiff(st, []tomb.TeamsMsg{tmsg("400", "Akshay", "v2")}, testChat())
	if len(ev) != 1 || ev[0].Kind != "teams_edited" || ev[0].Tier != model.TierWake {
		t.Fatalf("edit gave %+v", ev)
	}
	if items["teams-400"].Body != "v2" {
		t.Fatalf("edit inbox body = %q", items["teams-400"].Body)
	}
	if ev, _ := teamsDiff(st, []tomb.TeamsMsg{tmsg("400", "Akshay", "v2")}, testChat()); len(ev) != 0 {
		t.Fatalf("an unchanged edit re-announced: %+v", ev)
	}
}

func TestTeamsDeletes(t *testing.T) {
	st := baseline(t, tmsg("500", "Akshay", "oops"))
	d := tmsg("500", "Akshay", "")
	d.Properties = map[string]any{"deletetime": "1792621672158"}
	ev, _ := teamsDiff(st, []tomb.TeamsMsg{d}, testChat())
	if len(ev) != 1 || ev[0].Kind != "teams_deleted" || ev[0].Tier != model.TierRecord {
		t.Fatalf("delete gave %+v", ev)
	}
	// Created and deleted between two rounds: nothing to say.
	ev, items := teamsDiff(st, []tomb.TeamsMsg{d, func() tomb.TeamsMsg { m := d; m.ID = "501"; return m }()}, testChat())
	if len(ev) != 0 || len(items) != 0 {
		t.Fatalf("born-dead message gave %+v %v", ev, items)
	}
}

func TestTeamsSeenIsPrunedOldestFirst(t *testing.T) {
	st := &model.TeamsState{Seen: map[string]string{}}
	var msgs []tomb.TeamsMsg
	for i := 0; i < teamsSeenMax+20; i++ {
		msgs = append(msgs, tmsg(fmt.Sprint(1792621000000+i), "A", fmt.Sprint(i)))
	}
	teamsDiff(st, msgs, testChat())
	if len(st.Seen) != teamsSeenMax {
		t.Fatalf("seen = %d; want %d", len(st.Seen), teamsSeenMax)
	}
	if _, ok := st.Seen["1792621000000"]; ok {
		t.Fatal("oldest id survived pruning")
	}
	if _, ok := st.Seen[fmt.Sprint(1792621000000+teamsSeenMax+19)]; !ok {
		t.Fatal("newest id was pruned")
	}
}

// ---------- tomb: fake exec ----------

func fakeTomb(t *testing.T, scenario string) *tomb.Tomb {
	t.Helper()
	orig := forge.ExecCommandContext
	forge.ExecCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestTombHelperProcess", "--", scenario}, args...)...)
	}
	t.Cleanup(func() { forge.ExecCommandContext = orig })
	return &tomb.Tomb{Bin: "unused", Timeout: 10 * time.Second}
}

// TestTombHelperProcess is the fake tomb; gated on "--" in argv like TestHelperProcess.
func TestTombHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		return
	}
	scenario := args[1]
	switch scenario {
	case "ok":
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"<p>x</p>"}]}`)
		os.Exit(0)
	case "ok2":
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"<p>x</p>"},` +
			`{"id":"2","messagetype":"RichText/Html","imdisplayname":"Akshay","content":"<p>please look</p>"}]}`)
		os.Exit(0)
	case "ok3", "ok4":
		n := scenario[2:]
		os.Stdout.WriteString(`{"messages":[{"id":"1","messagetype":"RichText/Html","imdisplayname":"A","content":"<p>x</p>"},` +
			`{"id":"2","messagetype":"RichText/Html","imdisplayname":"Akshay","content":"<p>please look</p>"},` +
			`{"id":"` + n + `","messagetype":"RichText/Html","imdisplayname":"Akshay","content":"<p>again ` + n + `</p>"}]}`)
		os.Exit(0)
	case "gone404":
		os.Stderr.WriteString("Error: chat API returned 404\n")
	case "gone403":
		os.Stderr.WriteString("Error: chat API returned 403\n")
	case "auth403":
		os.Stderr.WriteString("Error: auth service returned 403: {\"error\":\"interaction_required\"}\n")
	case "down":
		os.Stderr.WriteString("Error: dial unix /run/user/1000/tomb.sock: connect: connection refused\n")
	case "garbage":
		os.Stdout.WriteString("not json")
		os.Exit(0)
	}
	os.Exit(1)
}

// ---------- live: one teams critic through baseline, news and gone ----------

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
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-teams-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: testChat()}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func(scenario string) error {
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"tomb": Plugin{Tomb: fakeTomb(t, scenario)}.Handle}}
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
	orig := teamsBlindAfter
	teamsBlindAfter = 0
	t.Cleanup(func() { teamsBlindAfter = orig })
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
	teamsBlindAfter = orig

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

const jackID = "8:orgid:bc3cb0f4-1239-4ec7-a528-4913522f79c5"

func from(m tomb.TeamsMsg, id string) tomb.TeamsMsg {
	m.FromURL = "https://noam-pilot2.msgapi.teams.microsoft.com/v1/users/ME/contacts/" + id
	return m
}

// Teams leaves imdisplayname blank on some rows (every row of a self-chat).
// The name learned from the same sender's other rows fills it, and quiet
// still matches.
func TestTeamsBlankNameUsesLearnedNameAndStaysQuiet(t *testing.T) {
	st := baseline(t, from(tmsg("600", "Jack Kelliher", "named row"), jackID))
	ev, items := teamsDiff(st, []tomb.TeamsMsg{from(tmsg("601", "", "blank row"), jackID)}, testChat())
	if len(ev) != 1 || ev[0].Author != "Jack Kelliher" || teamsRules(t, ev, trigger.Teams())["601"].Tier != model.TierRecord {
		t.Fatalf("blank-name row from a quiet sender: %+v", ev)
	}
	if it := items["teams-601"]; it.By != "Jack Kelliher" || it.FromID != jackID {
		t.Fatalf("inbox item: %+v", it)
	}
}

func TestTeamsUnknownBlankSenderShowsIDAndOperatorMatchesByID(t *testing.T) {
	chat := testChat()
	st := &model.TeamsState{}
	teamsDiff(st, nil, chat)
	ev, _ := teamsDiff(st, []tomb.TeamsMsg{
		from(tmsg("700", "", "self note"), jackID),
		from(tmsg("701", "", "someone else"), "8:orgid:00000000-0000-0000-0000-000000000001"),
	}, chat)
	if len(ev) != 2 {
		t.Fatalf("events: %+v", ev)
	}
	byID := teamsRules(t, ev, trigger.Teams())
	if e := byID["700"]; e.Author != jackID || e.Tier != model.TierRecord {
		t.Fatalf("operator by id with no name: %+v", e)
	}
	// Control: an id that is not the operator still wakes, and is named by its id.
	if e := byID["701"]; e.Author != "8:orgid:00000000-0000-0000-0000-000000000001" || e.Tier != model.TierWake {
		t.Fatalf("non-operator unknown sender: %+v", e)
	}
}

func TestTeamsTokenNameFallback(t *testing.T) {
	st := baseline(t)
	m := from(tmsg("800", "", "x"), "8:orgid:00000000-0000-0000-0000-000000000002")
	m.TokenName = "Liang Shi"
	if ev, _ := teamsDiff(st, []tomb.TeamsMsg{m}, testChat()); len(ev) != 1 || ev[0].Author != "Liang Shi" {
		t.Fatalf("token name not used: %+v", ev)
	}
}

// The operator never wakes a teams seat under the default rules, with no per-critic list (cheroot 2026-11-09): his
// message is recorded in the inbox; a non-operator message still wakes; and a figaro-sent message, which goes out
// under the operator's account carrying the disclosure footer, wakes (cheroot 2026-11-09: the secretary relies on it).
func TestTeamsOperatorRecordedButDisclosureWakes(t *testing.T) {
	chat := testChat()
	st := &model.TeamsState{}
	teamsDiff(st, nil, chat)
	ev, items := teamsDiff(st, []tomb.TeamsMsg{
		from(tmsg("900", "Jack Kelliher", "boss note"), trigger.OperatorSender),
		from(tmsg("901", "Liang Shi", "please look"), "8:orgid:00000000-0000-0000-0000-000000000009"),
		from(tmsg("902", "Jack Kelliher", "<p>build is red</p><hr><p>Agent-Authored on behalf of Jack Kelliher.</p>"), trigger.OperatorSender),
		from(tmsg("903", "", "x"), strings.ToUpper(trigger.OperatorSender)),
	}, chat)
	by := teamsRules(t, ev, trigger.Teams())
	if e := by["900"]; e.Tier != model.TierRecord {
		t.Fatalf("operator message tier %v; want TierRecord (no wake)", e.Tier)
	}
	if _, ok := items["teams-900"]; !ok {
		t.Fatal("operator message missing from the inbox")
	}
	if e := by["901"]; e.Tier != model.TierWake {
		t.Fatalf("control: a non-operator message must still wake, tier %v", e.Tier)
	}
	if e := by["902"]; e.Tier != model.TierWake {
		t.Fatalf("a figaro-sent message (disclosure footer, operator account) must wake, tier %v", e.Tier)
	}
	if e := by["903"]; e.Tier != model.TierRecord {
		t.Fatalf("upper-cased operator id woke: %+v", e)
	}
}

// A deletion is recorded, never a wake, under the default rules.
func TestTeamsDeletionRecordedByDefault(t *testing.T) {
	st := baseline(t, tmsg("950", "Akshay", "v1"))
	d := tmsg("950", "Akshay", "")
	d.Properties = map[string]any{"deletetime": "1"}
	ev, _ := teamsDiff(st, []tomb.TeamsMsg{d}, testChat())
	if len(ev) != 1 || ev[0].Kind != "teams_deleted" {
		t.Fatalf("no deletion event: %+v", ev)
	}
	if e := teamsRules(t, ev, trigger.Teams())["950"]; e.Tier != model.TierRecord {
		t.Fatalf("deletion woke: %+v", e)
	}
}

// ---------- live: trigger rules on a real role form ----------

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
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-trigger-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
		CreatedAt: time.Now().UTC(), FormID: id, Chat: testChat()}
	if err := st.SaveCritic(ctx, w); err != nil {
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
		p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"tomb": Plugin{Tomb: fakeTomb(t, scenario)}.Handle}}
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

// ---------- live: collect mode (W7 item 2) ----------

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
		t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
		w := &model.Critic{Name: "livetest-" + c.name + "-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeTeams, Source: "teams-chat",
			CreatedAt: time.Now().UTC(), FormID: id, Chat: testChat(), PingEverySec: c.ping}
		w.Runtime.LastPing = time.Now().UTC() // a ping just went out: the next is 30 minutes away
		if err := st.SaveCritic(ctx, w); err != nil {
			t.Fatal(err)
		}
		round := func(scenario string) {
			t.Helper()
			var out strings.Builder
			p := &poll.Poller{Store: st, Fig: f, Out: &out, Plugins: plugin.InProc{"tomb": Plugin{Tomb: fakeTomb(t, scenario)}.Handle}}
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

func fromMsg(id, from, fromID, content string) tomb.TeamsMsg {
	m := tmsg(id, from, content)
	m.FromURL = "https://x/v1/users/ME/contacts/" + fromID
	return m
}

// Self messages (chat.self): a new one is one compact item with its id kept
// (inbox_fetch reads the full text); its edits and deletes write nothing.
// The control is the same sequence from another sender.
func TestTeamsSelfMessagesCompactAndNeverUpdated(t *testing.T) {
	const self, other = "8:orgid:self", "8:orgid:other"
	chat := &model.ChatRef{ID: "19:abc@thread.v2", Topic: "DM", Self: self}
	long := "status: everything that needs Chiqun now<br>" + strings.Repeat("a line of detail<br>", 40)
	st := &model.TeamsState{}
	teamsDiff(st, []tomb.TeamsMsg{fromMsg("1", "Jack", self, "old")}, chat) // first round records

	ev, items := teamsDiff(st, []tomb.TeamsMsg{fromMsg("1", "Jack", self, "old"), fromMsg("2", "Jack", self, long), fromMsg("3", "Akshay", other, long)}, chat)
	s, o := items["teams-2"], items["teams-3"]
	if !s.Self || s.Body != "status: everything that needs Chiqun now" || !s.Cut || s.FromID != "" || len(ev) != 2 {
		t.Fatalf("self new: %+v (events %d)", s, len(ev))
	}
	if o.Self || len(o.Body) < 500 || o.FromID != other {
		t.Fatalf("control, other new: %+v", o)
	}

	edited := strings.Replace(long, "everything", "all", 1)
	ev, items = teamsDiff(st, []tomb.TeamsMsg{fromMsg("2", "Jack", self, edited), fromMsg("3", "Akshay", other, edited)}, chat)
	if _, ok := items["teams-2"]; ok || len(ev) != 1 || ev[0].Comment != "3" || items["teams-3"].Kind != "teams_edited" {
		t.Fatalf("edit: self must write nothing, other must: items %v events %+v", items, ev)
	}

	del := func(m tomb.TeamsMsg) tomb.TeamsMsg {
		m.Properties = map[string]any{"deletetime": "1794500000000"}
		return m
	}
	ev, items = teamsDiff(st, []tomb.TeamsMsg{del(fromMsg("2", "Jack", self, "")), del(fromMsg("3", "Akshay", other, ""))}, chat)
	if _, ok := items["teams-2"]; ok || items["teams-3"].Kind != "teams_deleted" || items["teams-3"].Body != "" {
		t.Fatalf("delete: self nothing, other one teams_deleted: %v", items)
	}
	_ = ev

	// control: without chat.self, Jack's messages are full items like anyone's.
	plain := &model.ChatRef{ID: chat.ID}
	st2 := &model.TeamsState{}
	teamsDiff(st2, nil, plain)
	_, items = teamsDiff(st2, []tomb.TeamsMsg{fromMsg("5", "Jack", self, long)}, plain)
	if it := items["teams-5"]; it.Self || len(it.Body) < 500 {
		t.Fatalf("control, no chat.self: %+v", it)
	}
}

// A successful round names inbox_fetch for the chat (the one place the
// fetch commands live); a failed read writes no inbox_fetch (control).
func TestTeamsRoundSetsInboxFetch(t *testing.T) {
	w := &model.Critic{Name: "c", Chat: testChat()}
	rep, err := round(context.Background(), fakeTomb(t, "ok"), w)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]string
	if json.Unmarshal(rep.Set["inbox_fetch"], &f) != nil || f["chat"] != "19:abc@thread.v2" || !strings.Contains(f["text"], "<id>") {
		t.Fatalf("inbox_fetch = %s", rep.Set["inbox_fetch"])
	}
	rep, _ = round(context.Background(), fakeTomb(t, "down"), &model.Critic{Name: "c", Chat: testChat()})
	if rep == nil || rep.Set["inbox_fetch"] != nil {
		t.Fatalf("control: a failed read set inbox_fetch: %+v", rep)
	}
}
