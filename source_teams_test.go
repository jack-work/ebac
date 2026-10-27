package main

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
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/tomb"
)

func tmsg(id, from, content string) tomb.TeamsMsg {
	return tomb.TeamsMsg{ID: id, Version: id, ComposeTime: "2026-10-21T22:00:00Z", MessageType: "RichText/Html", From: from, Content: content}
}

func testChat() *model.ChatRef {
	return &model.ChatRef{ID: "19:abc@thread.v2", Topic: "MD1 + Cowork", Quiet: []string{"Jack Kelliher"}}
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
	if !it.HasImage || !strings.Contains(it.Fetch, "--download-images") || !strings.Contains(it.Fetch, `select(.id=="200")`) {
		t.Fatalf("image/fetch wrong: %+v", it)
	}
	if it.Chat != "19:abc@thread.v2" || it.By != "Liang Shi" {
		t.Fatalf("item = %+v", it)
	}
}

func TestTeamsQuietSenderIsRecordedNotWoken(t *testing.T) {
	st := baseline(t)
	ev, items := teamsDiff(st, []tomb.TeamsMsg{tmsg("300", "jack kelliher", "note to self")}, testChat())
	if len(ev) != 1 || ev[0].Tier != model.TierRecord {
		t.Fatalf("quiet sender (case-insensitive) must be TierRecord: %+v", ev)
	}
	if _, ok := items["teams-300"]; !ok {
		t.Fatal("quiet sender's message must still reach the inbox")
	}
	if (&model.Delta{Events: ev}).WakeWorthy() {
		t.Fatal("a quiet-only round is wake-worthy")
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
	st, err := NewStore(f)
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
		p := &Poller{Store: st, Fig: f, Out: &out, Tomb: fakeTomb(t, scenario)}
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
	if len(ev) != 1 || ev[0].Author != "Jack Kelliher" || ev[0].Tier != model.TierRecord {
		t.Fatalf("blank-name row from a quiet sender: %+v", ev)
	}
	if it := items["teams-601"]; it.By != "Jack Kelliher" || it.FromID != jackID {
		t.Fatalf("inbox item: %+v", it)
	}
}

func TestTeamsUnknownBlankSenderShowsIDAndQuietMatchesByID(t *testing.T) {
	chat := testChat()
	chat.Quiet = []string{"bc3cb0f4-1239-4ec7-a528-4913522f79c5"} // bare guid
	st := &model.TeamsState{}
	teamsDiff(st, nil, chat)
	ev, _ := teamsDiff(st, []tomb.TeamsMsg{
		from(tmsg("700", "", "self note"), jackID),
		from(tmsg("701", "", "someone else"), "8:orgid:00000000-0000-0000-0000-000000000001"),
	}, chat)
	if len(ev) != 2 {
		t.Fatalf("events: %+v", ev)
	}
	byID := map[string]model.Event{ev[0].Comment: ev[0], ev[1].Comment: ev[1]}
	if e := byID["700"]; e.Author != jackID || e.Tier != model.TierRecord {
		t.Fatalf("quiet by guid with no name: %+v", e)
	}
	// Control: an id that is not quiet still wakes, and is named by its id.
	if e := byID["701"]; e.Author != "8:orgid:00000000-0000-0000-0000-000000000001" || e.Tier != model.TierWake {
		t.Fatalf("non-quiet unknown sender: %+v", e)
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
