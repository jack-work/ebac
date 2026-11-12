package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

const (
	knight    = "@8d70861a00000000000000000000aaaa"
	lead      = "@cc540006000000000000000000000bbb"
	secretary = "@3dbeb326000000000000000000000ccc"
)

func who(l string) map[string]any { return map[string]any{"__typename": "User", "login": l} }
func bot(l string) map[string]any { return map[string]any{"__typename": "Bot", "login": l} }

// s5World: issue 301 (ours, labelled) is linked from 101 (ours -> the Knight), 104 (ours, ledger -> the ledger lead),
// 201 (Chiqun's -> the Secretary) and 105 (ours, merged: never pinged).
func s5World(t *testing.T) *world {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, TitleMatch: `(?i)ledger`, Role: lead}, {Author: ours, Role: knight}, {Author: his, Role: secretary}}
	link := "Part of https://x/bic/aether/issues/301"
	old := w.now.Add(-time.Hour)
	w.gh.items[101].body, w.gh.items[201].body = link, link
	w.gh.items[104] = &item{n: 104, author: ours, title: "[Effort][ledger-2] the ledger piece", state: "OPEN", head: headA, mergeable: "MERGEABLE", updated: old, body: link}
	w.gh.items[105] = &item{n: 105, author: ours, title: "[Effort][1-1] merged", state: "OPEN", head: headA, mergeable: "MERGEABLE", updated: old, body: link}
	w.gh.items[301].body = "The plan item.\n\n- [ ] one\n- [x] two\n\n<!-- plan-sync -->\nPlan: owner Knight.\n- [ ] synced step\n<!-- /plan-sync -->\nhuman tail\n"
	w.pass(0)
	m := w.gh.items[105]
	m.state, m.mergedAt, m.updated = "MERGED", w.now.Format(time.RFC3339), w.now.Add(time.Minute)
	w.pass(2 * time.Minute)
	return w
}

// change applies f to issue 301 and bumps it, then runs a pass.
func (w *world) change(f func(is *item)) Result {
	is := w.gh.items[301]
	f(is)
	is.updated = w.now.Add(time.Minute)
	return w.pass(2 * time.Minute)
}

func pingsTo(r Result) map[string]string {
	out := map[string]string{}
	for _, p := range r.Pings {
		out[p.To] = p.Text
	}
	return out
}

// S5: a material change by a human reaches the owner role of each OPEN PR linking the issue, one line each, naming
// what changed and who; an edit confined to a plan-sync block (the sync edits as John-Kelliher) and a bot's label
// reach nobody.
func TestIssueChangeReachesOwners(t *testing.T) {
	w := s5World(t)

	r := w.change(func(is *item) {
		is.comments = append(is.comments, map[string]any{"author": who("alice"), "createdAt": w.now.Format(time.RFC3339)})
	})
	got := pingsTo(r)
	if len(r.Pings) != 3 || !strings.Contains(got[knight], "#101") || !strings.Contains(got[lead], "#104") || !strings.Contains(got[secretary], "#201") {
		t.Fatalf("human comment: pings %v", r.Pings)
	}
	for to, text := range got {
		if !strings.Contains(text, "issue #301") || !strings.Contains(text, "comment by alice") || strings.Contains(text, "#105") ||
			strings.ContainsAny(text, "\n\r") || len(text) > 400 {
			t.Fatalf("ping to %s: %q", to, text)
		}
	}
	if d := w.st.Issues[301].D; d.HumanComments != 1 || entry(t, r, "issue:301")["dg"] != nil {
		t.Fatalf("digests %+v (state only, not on the entry)", d)
	}

	// The sync (as John-Kelliher) rewrites its block and ticks a checklist item inside it: nobody is pinged.
	r = w.change(func(is *item) {
		is.body = strings.Replace(is.body, "Plan: owner Knight.\n- [ ] synced step", "Plan: owner Knight, needs #4100.\n- [x] synced step", 1)
		is.editor = who(ours)
	})
	if len(r.Pings) != 0 {
		t.Fatalf("sync block edit: pings %v", r.Pings)
	}
	// The same login edits outside the block: news.
	r = w.change(func(is *item) {
		is.body = strings.Replace(is.body, "human tail", "human tail, amended", 1)
	})
	if text := pingsTo(r)[knight]; !strings.Contains(text, "changed: body by "+ours) {
		t.Fatalf("edit outside the block: %v", r.Pings)
	}
	r = w.change(func(is *item) {
		is.labels = append(is.labels, "triaged")
		is.events = append(is.events, map[string]any{"__typename": "LabeledEvent", "createdAt": w.now.Format(time.RFC3339), "actor": bot("automaton[bot]")})
		is.comments = append(is.comments, map[string]any{"author": bot("ci"), "createdAt": w.now.Format(time.RFC3339)})
	})
	if len(r.Pings) != 0 || r.Set["issue:301"] == nil {
		t.Fatalf("bot label: pings %v, entry written %v", r.Pings, r.Set["issue:301"] != nil)
	}

	// A human ticks the checklist: checklist, not body.
	r = w.change(func(is *item) {
		is.body = strings.Replace(is.body, "- [ ] one", "- [x] one", 1)
		is.editor = who("bob")
	})
	if text := pingsTo(r)[knight]; !strings.Contains(text, "changed: checklist by bob") {
		t.Fatalf("checklist tick: %q", text)
	}
	// A human relabels (case and order only: no change), then closes it.
	r = w.change(func(is *item) {
		is.labels = []string{"TRIAGED", "Effort-Router"}
		is.events = append(is.events, map[string]any{"__typename": "LabeledEvent", "createdAt": w.now.Format(time.RFC3339), "actor": who("carol")})
	})
	if len(r.Pings) != 0 {
		t.Fatalf("a label re-case pinged: %v", r.Pings)
	}
	r = w.change(func(is *item) {
		is.state, is.closedAt = "CLOSED", w.now.Format(time.RFC3339)
		is.events = append(is.events, map[string]any{"__typename": "ClosedEvent", "createdAt": w.now.Format(time.RFC3339), "actor": who("carol")})
	})
	if text := pingsTo(r)[secretary]; !strings.Contains(text, "changed: state closed by carol") {
		t.Fatalf("close: %v", r.Pings)
	}
}

// A newly tracked issue is a baseline, never news; an issue no open tracked PR links pings nobody (said in the log).
func TestIssueBaselineAndUnlinked(t *testing.T) {
	w := s5World(t)
	w.gh.items[3020] = &item{n: 3020, issue: true, author: ours, title: "new", labels: []string{"effort-router"}, state: "OPEN",
		updated: w.now.Add(time.Minute), comments: []map[string]any{{"author": who("alice"), "createdAt": w.now.Format(time.RFC3339)}}}
	r := w.pass(2 * time.Minute)
	if r.Set["issue:3020"] == nil || len(r.Pings) != 0 {
		t.Fatalf("baseline: set %v pings %v", setKeys(r), r.Pings)
	}
	w.gh.items[3020].comments = append(w.gh.items[3020].comments, map[string]any{"author": who("alice"), "createdAt": w.now.Format(time.RFC3339)})
	w.gh.items[3020].updated = w.now.Add(time.Minute)
	r = w.pass(2 * time.Minute)
	if len(r.Pings) != 0 || !strings.Contains(strings.Join(r.Log, "\n"), "issue #3020 changed (comment by alice): no open tracked PR links it") {
		t.Fatalf("unlinked: pings %v log %v", r.Pings, r.Log)
	}
}

// At most MaxIssuePings a pass; the rest wait in the queue, oldest first, and go out on the next pass, which still
// costs one GitHub call.
func TestIssuePingsCappedAndQueued(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: knight}}
	var body []string
	for i := 0; i < 10; i++ {
		n := 4000 + i
		w.gh.items[n] = &item{n: n, issue: true, author: ours, title: fmt.Sprint("issue ", n), labels: []string{"effort-router"}, state: "OPEN", updated: w.now}
		body = append(body, fmt.Sprintf("#%d", n))
	}
	w.gh.items[101].body = strings.Join(body, " ")
	w.pass(0)
	for i := 0; i < 10; i++ {
		is := w.gh.items[4000+i]
		is.comments = []map[string]any{{"author": who("alice"), "createdAt": w.now.Format(time.RFC3339)}}
		is.updated = w.now.Add(time.Minute)
	}
	r := w.pass(2 * time.Minute)
	if len(r.Pings) != MaxIssuePings || len(w.st.Queue) != 10-MaxIssuePings || !strings.Contains(r.Pings[0].Text, "#4000") {
		t.Fatalf("pass 1: %d pings, %d queued", len(r.Pings), len(w.st.Queue))
	}
	r = w.pass(2 * time.Minute)
	if len(r.Pings) != 10-MaxIssuePings || len(w.st.Queue) != 0 || r.GHCalls != 1 || !strings.Contains(r.Pings[3].Text, "#4009") {
		t.Fatalf("pass 2: %d pings, %d queued, %d calls", len(r.Pings), len(w.st.Queue), r.GHCalls)
	}
	if r := w.pass(2 * time.Minute); len(r.Pings) != 0 {
		t.Fatalf("pass 3 re-sent: %v", r.Pings)
	}
}

func TestQueueBounded(t *testing.T) {
	st := State{}
	var fresh []plugin.Ping
	for i := 0; i < MaxQueue+20; i++ {
		fresh = append(fresh, plugin.Ping{To: knight, Text: fmt.Sprint(i)})
	}
	var log []string
	sent := queuePings(&st, fresh, func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })
	if len(sent) != MaxIssuePings || len(st.Queue) != MaxQueue-MaxIssuePings || sent[0].Text != "20" || len(log) != 1 {
		t.Fatalf("sent %d (first %q), queued %d, log %v", len(sent), sent[0].Text, len(st.Queue), log)
	}
}

// Digests: the checklist is its own digest (a tick moves it, not body's); labels are case- and order-blind; human
// comments leave out bots.
func TestIssueDigests(t *testing.T) {
	base := Issue{Body: "Text.\r\n\r\n- [ ] a\r\n* [X] b\r\n", Labels: []string{"B", "a"},
		Comments: []Comment{{By: &login{Login: "alice"}, At: "1"}, {By: &login{Typename: "Bot", Login: "x"}, At: "3"},
			{By: &login{Login: "y[bot]"}, At: "5"}, {By: &login{Login: "bob"}, At: "2"}}}
	d, last := issueDigests(base)
	if d.HumanComments != 2 || last != "2" {
		t.Fatalf("humans %d last %q", d.HumanComments, last)
	}
	tick := base
	tick.Body = strings.Replace(base.Body, "- [ ] a", "- [x] a", 1)
	tick.Labels = []string{"A", "b"}
	d2, _ := issueDigests(tick)
	if d2.Body != d.Body || d2.Checklist == d.Checklist || d2.Labels != d.Labels {
		t.Fatalf("tick: %+v vs %+v", d2, d)
	}
	text := base
	text.Body = "Other text.\n\n- [ ] a\n* [X] b\n"
	if d3, _ := issueDigests(text); d3.Body == d.Body || d3.Checklist != d.Checklist {
		t.Fatalf("text edit: %+v vs %+v", d3, d)
	}
	var back IssueDigests
	if err := json.Unmarshal(model.MustJSON(d), &back); err != nil || back != d || len(d.String()) != 28 || !strings.HasSuffix(d.String(), ".2") {
		t.Fatalf("digests %q round-trip %+v %v", d.String(), back, err)
	}
}

func TestOwnerRules(t *testing.T) {
	if err := CheckOwners([]model.OwnerRule{{Author: ours, Role: "knight"}}); err == nil {
		t.Fatal("a role that is not @<hex> was accepted")
	}
	if err := CheckOwners([]model.OwnerRule{{Author: ours, TitleMatch: "([", Role: knight}}); err == nil {
		t.Fatal("a bad title regex was accepted")
	}
	o, _ := compileOwners([]model.OwnerRule{{Author: ours, TitleMatch: `(?i)ledger`, Role: lead}, {Author: ours, Role: knight}})
	if r := roleOf(o, "john-kelliher", "[Effort][LEDGER-1] x"); r != lead {
		t.Fatalf("ledger PR -> %q", r)
	}
	if r := roleOf(o, ours, "[Effort][cache-1] x"); r != knight {
		t.Fatalf("other PR -> %q", r)
	}
	if r := roleOf(o, his, "ledger"); r != "" {
		t.Fatalf("Chiqun's PR with no rule -> %q", r)
	}
}

// The comment read is a window (the last 50): a new human comment that pushes an older human one out leaves the count
// where it was and is still news.
func TestHumanCommentPastTheWindow(t *testing.T) {
	w := s5World(t)
	w.change(func(is *item) {
		for i := 0; i < 50; i++ {
			is.comments = append(is.comments, map[string]any{"author": who("alice"), "createdAt": w.now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
		}
	})
	r := w.change(func(is *item) {
		is.comments = append(is.comments, map[string]any{"author": who("dave"), "createdAt": w.now.Add(time.Hour).Format(time.RFC3339)})
	})
	if !strings.Contains(pingsTo(r)[knight], "comment by dave") {
		t.Fatalf("a comment past the window: %v", r.Pings)
	}
}

// The plugin hands the pass's pings to the core.
func TestHandleCarriesPings(t *testing.T) {
	w := s5World(t)
	now := w.now
	pl := Plugin{GH: w.gh, Hub: w.hub, Now: func() time.Time { return now }}
	c := model.Critic{Name: "effort-github", Source: model.SourceScout, Scout: &w.watch}
	c.Runtime.Scout = model.MustJSON(w.st)
	w.gh.items[301].comments = []map[string]any{{"author": who("alice"), "createdAt": now.Format(time.RFC3339)}}
	w.gh.items[301].updated = now.Add(time.Minute)
	now = now.Add(2 * time.Minute)
	rep, err := pl.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: c, Inputs: w.in})
	if err != nil || len(rep.Pings) != 3 || !strings.Contains(strings.Join(rep.Log, " "), "3 ping(s), 0 queued") {
		t.Fatalf("err %v pings %v log %v", err, rep.Pings, rep.Log)
	}
}

// The plan-sync markers are whole lines (whitespace trimmed): what sits between them, the marker lines included, is
// left out of the body and checklist digests; an unclosed block runs to the end; a marker inside other text is text.
func TestPlanSyncBlocks(t *testing.T) {
	dig := func(body string) IssueDigests {
		d, _ := issueDigests(Issue{Body: body})
		return d
	}
	const doc = "Human text.\n- [ ] human step\n  <!-- plan-sync -->  \nPlan text.\n- [ ] plan step\n<!-- /plan-sync -->\nMore human text.\n"
	base := dig(doc)
	same := func(name, body string) {
		t.Helper()
		if d := dig(body); d.Body != base.Body || d.Checklist != base.Checklist {
			t.Errorf("%s moved the digests", name)
		}
	}
	moved := func(name, body string, bodyMoves, checkMoves bool) {
		t.Helper()
		d := dig(body)
		if (d.Body != base.Body) != bodyMoves || (d.Checklist != base.Checklist) != checkMoves {
			t.Errorf("%s: body moved %v (want %v), checklist moved %v (want %v)", name, d.Body != base.Body, bodyMoves,
				d.Checklist != base.Checklist, checkMoves)
		}
	}
	same("an edit inside the block", strings.Replace(doc, "Plan text.", "Plan text, rewritten.", 1))
	same("a checklist tick inside the block", strings.Replace(doc, "- [ ] plan step", "- [x] plan step", 1))
	same("CRLF", strings.ReplaceAll(doc, "\n", "\r\n"))
	moved("an edit outside", strings.Replace(doc, "More human text.", "More human text!", 1), true, false)
	moved("a tick outside", strings.Replace(doc, "- [ ] human step", "- [x] human step", 1), false, true)

	// Unclosed: everything after the opening line is the sync's.
	open := "Human text.\n<!-- plan-sync -->\nPlan.\n- [ ] a\n"
	d0 := dig(open)
	if d := dig(open + "more plan\n- [x] b\n"); d != d0 {
		t.Error("an edit after an unclosed marker moved the digests")
	}
	if d := dig(strings.Replace(open, "Human text.", "Human text!", 1)); d.Body == d0.Body {
		t.Error("an edit before an unclosed marker did not move the body digest")
	}

	// A marker inside other text is not a marker: what follows it is human text.
	inline := "Human text <!-- plan-sync --> still human.\nTail.\n<!-- /plan-sync -->\n"
	if d := dig(strings.Replace(inline, "Tail.", "Tail!", 1)); d.Body == dig(inline).Body {
		t.Error("an inline marker opened a block")
	}
	if got := stripSync("a\n <!-- plan-sync -->\t\nb\n<!-- /plan-sync -->\nc\nx <!-- /plan-sync -->"); got != "a\nc\nx <!-- /plan-sync -->" {
		t.Errorf("stripSync: %q", got)
	}
}
