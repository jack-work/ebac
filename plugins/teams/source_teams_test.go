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

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/tomb"
	"github.com/gluck/ebac/sdk/trigger"
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
	kept, dropped := rules.Apply(ev, nil)
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

// The operator never wakes a teams seat under the default rules, with no per-critic list (ops 2026-11-09): his
// message is recorded in the inbox; a non-operator message still wakes; and a figaro-sent message, which goes out
// under the operator's account carrying the disclosure footer, wakes (ops 2026-11-09: the secretary relies on it).
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

// ---------- live: collect mode (W7 item 2) ----------

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
	if !s.Self || s.Body != "status: everything that needs Chiqun now" || !s.Cut || s.FromID != "" || s.By != "" || s.Time != "" || len(ev) != 2 {
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
