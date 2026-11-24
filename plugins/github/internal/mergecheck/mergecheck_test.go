package mergecheck

import (
	"strings"
	"testing"
)

const (
	head  = "aaaaaaaa11111111111111111111111111111111"
	old   = "bbbbbbbb22222222222222222222222222222222"
	tree  = "7777777733333333333333333333333333333333"
	other = "8888888844444444444444444444444444444444"
)

// base is an open PR at head with Runtime CI still running and nothing else: mergeable, no conflict.
func base() In {
	return In{Facts: Facts{Oid: head, Tree: tree, State: "OPEN", Heavy: true, Mergeable: "MERGEABLE", MergeState: "BLOCKED",
		Contexts: []Context{{Name: Runtime + " (Build)", State: "PENDING"}}}, Files: []string{"aether_rust/x.rs"}}
}

func state(c In) string {
	s := StateOf(&c.Facts, Checklist(c))
	if b := ArchBlock(&c.Facts, c.Arch); b != "" && s != "waiting: conflict" {
		s = b
	}
	return s
}

func rev(login, st, oid, tr string) Review {
	return Review{Login: login, State: st, Oid: oid, Tree: tr}
}

// rule.merge_ready: ONE general bot approval at the head (or a same-tree head) is ready, checks never waited on.
func TestBotApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*In)
		want string
	}{
		{"no approval", func(*In) {}, "waiting: bot approval"},
		{"lukas approved the head, Runtime CI pending", func(c *In) { c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", head, tree)} }, "ready"},
		{"automaton review", func(c *In) { c.Facts.Reviews = []Review{rev(Automaton, "APPROVED", head, "")} }, "ready"},
		{"Ilya review", func(c *In) { c.Facts.Reviews = []Review{rev(Ilya, "APPROVED", head, "")} }, "ready"},
		{"a human approval is not a bot's", func(c *In) { c.Facts.Reviews = []Review{rev("Chiqun-Zhang", "APPROVED", head, tree)} }, "waiting: bot approval"},
		{"bot changes requested", func(c *In) { c.Facts.Reviews = []Review{rev(Lukas, "CHANGES_REQUESTED", head, tree)} }, "waiting: bot approval"},
		// A written verdict counts whatever GitHub records (the architect's comment; the automaton's marker).
		{"architect written APPROVE", func(c *In) { c.Arch = &Arch{Verdict: "APPROVE", Sha: head[:8]} }, "ready"},
		{"architect APPROVE_WITH_NOTES, 7-hex sha", func(c *In) { c.Arch = &Arch{Verdict: "APPROVE_WITH_NOTES", Sha: head[:7]} }, "ready"},
		{"automaton written approve", func(c *In) { c.Written = []Written{{By: Automaton, Verdict: "APPROVE", Sha: head[:8]}} }, "ready"},
		{"automaton written RC", func(c *In) { c.Written = []Written{{By: Automaton, Verdict: "REQUEST_CHANGES", Sha: head[:8]}} }, "waiting: bot approval"},
		// Another head: kept only when its tree is the head's.
		{"approval at an older head", func(c *In) { c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", old, other)} }, "waiting: bot approval"},
		{"review at a same-tree head", func(c *In) { c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", old, tree)} }, "ready"},
		{"written verdict at a same-tree head", func(c *In) { c.Arch = &Arch{Verdict: "APPROVE", Sha: old[:8], Tree: tree[:12]} }, "ready"},
		{"written verdict, tree not known", func(c *In) { c.Arch = &Arch{Verdict: "APPROVE", Sha: old[:8]} }, "waiting: bot approval"},
		{"head tree not known", func(c *In) {
			c.Facts.Tree = ""
			c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", old, tree)}
		}, "waiting: bot approval"},
		// An architect pass in progress is a pending wait only while nothing approves the head.
		{"hold, no approval", func(c *In) { c.Arch = &Arch{Hold: true, HoldAt: "t", HoldSha: head} }, "waiting: bot approval"},
		{"hold, lukas approved", func(c *In) {
			c.Arch = &Arch{Hold: true, HoldAt: "t", HoldSha: head}
			c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", head, tree)}
		}, "ready"},
	} {
		c := base()
		tc.mod(&c)
		if got := state(c); got != tc.want {
			t.Errorf("%s: %q, want %q (%+v)", tc.name, got, tc.want, Checklist(c))
		}
	}
	c := base()
	c.Arch = &Arch{Hold: true, HoldAt: "t"}
	if r := Checklist(c)[0]; r.Status != "pending" || !strings.Contains(r.Note, "architect pass in progress") {
		t.Fatalf("hold row: %+v", r)
	}
	c = base()
	c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", old, tree)}
	if r := Checklist(c)[0]; !strings.Contains(r.Note, "lukas (same tree)") {
		t.Fatalf("same-tree row: %+v", r)
	}
}

// A red at the head is ours until proven main-side; pending or absent never blocks.
func TestNoRed(t *testing.T) {
	ok := func(c *In) { c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", head, tree)} }
	for _, tc := range []struct {
		name string
		mod  func(*In)
		want string
	}{
		{"Runtime red", func(c *In) { c.Facts.Contexts = []Context{{Name: Runtime, State: "FAILURE"}} }, "waiting: no red of ours"},
		{"Runtime red proven main-side", func(c *In) {
			c.Facts.Contexts = []Context{{Name: Runtime, State: "FAILURE"}}
			c.Marks.Item = map[string]any{"red_not_ours": "main run 123 red too"}
		}, "ready"},
		{"Rust error", func(c *In) { c.Facts.Contexts = []Context{{Name: Rust, State: "ERROR"}} }, "waiting: no red of ours"},
		{"Rust red, rust_not_ours", func(c *In) {
			c.Facts.Contexts = []Context{{Name: Rust, State: "FAILURE"}}
			c.Marks.Row = map[string]any{"rust_not_ours": "flake"}
		}, "ready"},
		{"rust_not_ours does not cover Runtime", func(c *In) {
			c.Facts.Contexts = []Context{{Name: Runtime, State: "FAILURE"}}
			c.Marks.Row = map[string]any{"rust_not_ours": "flake"}
		}, "waiting: no red of ours"},
		{"check-ni FAIL", func(c *In) { c.Jobs = []Job{{ID: 7, Queue: "aether.check-ni", Verdict: "FAIL"}} }, "waiting: no red of ours"},
		{"check-ni running, older FAIL", func(c *In) {
			c.Jobs = []Job{{ID: 8, Queue: "aether.check-ni", State: "leased"}, {ID: 7, Queue: "aether.check-ni", Verdict: "FAIL"}}
		}, "ready"},
		{"check-ni not run", func(*In) {}, "ready"},
		{"opt-in gate FAIL is evidence only", func(c *In) { c.Jobs = []Job{{ID: 9, Queue: "aether.gate-kit", Verdict: "FAIL"}} }, "ready"},
		{"docs-only: check-ni not read", func(c *In) {
			c.Files = []string{"docs/a.md"}
			c.Jobs = []Job{{ID: 7, Queue: "aether.check-ni", Verdict: "FAIL"}}
		}, "ready"},
		{"other pipelines are not read", func(c *In) { c.Facts.Contexts = []Context{{Name: "license/cla", State: "FAILURE"}} }, "ready"},
	} {
		c := base()
		ok(&c)
		tc.mod(&c)
		if got := state(c); got != tc.want {
			t.Errorf("%s: %q, want %q (%+v)", tc.name, got, tc.want, Checklist(c))
		}
	}
}

// A conflict wins over every other step; UNKNOWN is GitHub recomputing (the last settled value stands).
func TestConflict(t *testing.T) {
	for _, tc := range []struct {
		mg, ms, last, want string
		approved           bool
	}{
		{"CONFLICTING", "DIRTY", "", "waiting: conflict", false},
		{"MERGEABLE", "DIRTY", "", "waiting: conflict", true},
		{"UNKNOWN", "UNKNOWN", "CONFLICTING", "waiting: conflict", false},
		{"UNKNOWN", "UNKNOWN", "MERGEABLE", "ready", true},
		{"UNKNOWN", "UNKNOWN", "", "ready", true},
		{"MERGEABLE", "BEHIND", "", "ready", true},
	} {
		c := base()
		c.Facts.Mergeable, c.Facts.MergeState, c.Facts.LastMergeable = tc.mg, tc.ms, tc.last
		c.Facts.Contexts = []Context{{Name: Runtime, State: "FAILURE"}}
		if tc.approved {
			c.Facts.Contexts = nil
			c.Facts.Reviews = []Review{rev(Lukas, "APPROVED", head, tree)}
		}
		if got := state(c); got != tc.want {
			t.Errorf("%s/%s last %q: %q, want %q", tc.mg, tc.ms, tc.last, got, tc.want)
		}
	}
	// A conflict beats an architect RC at the head too.
	c := base()
	c.Facts.Mergeable = "CONFLICTING"
	c.Arch = &Arch{Verdict: "REQUEST_CHANGES", Sha: head[:8], At: "2026-11-12T10:00:00Z"}
	if got := state(c); got != "waiting: conflict" {
		t.Fatalf("conflict over RC: %q", got)
	}
}

// The reviewer-conditions step is the Lead's ready.<pr>: shown, never blocking.
func TestConditionsManual(t *testing.T) {
	rows := Checklist(base())
	if len(rows) != len(Steps) {
		t.Fatalf("%d rows for %d steps", len(rows), len(Steps))
	}
	for i, s := range Steps {
		if rows[i].ID != s.ID || rows[i].Label != s.Label {
			t.Fatalf("row %d %+v, step %+v", i, rows[i], s)
		}
	}
	if r := rows[3]; r.ID != "conditions" || !r.OK || r.Status != "manual" {
		t.Fatalf("conditions: %+v", r)
	}
}

// An architect RC blocks while it names the head (or a same-tree head) and no later Ilya approval replaced it.
func TestArchBlock(t *testing.T) {
	open := &Facts{State: "OPEN", Oid: head, Tree: tree}
	rc := func(sha, tr string) *Arch {
		return &Arch{Verdict: "REQUEST_CHANGES", Sha: sha, Tree: tr, At: "2026-11-12T10:00:00Z"}
	}
	ilya := func(at string) *Facts {
		f := *open
		f.Reviews = []Review{{Login: Ilya, State: "APPROVED", At: at}}
		return &f
	}
	for _, tc := range []struct {
		name string
		f    *Facts
		a    *Arch
		want string
	}{
		{"RC at the head", open, rc(head[:8], ""), "blocked: architect RC at aaaaaaaa"},
		{"RC at a same-tree head", open, rc(old[:8], tree[:12]), "blocked: architect RC at bbbbbbbb"},
		{"RC at an older head: the fix is pushed", open, rc(old[:8], other[:12]), ""},
		{"RC at an older head, tree unknown", open, rc(old[:8], ""), ""},
		{"Ilya approved after", ilya("2026-11-12T12:00:00Z"), rc(head[:8], ""), ""},
		{"Ilya approved before", ilya("2026-11-12T09:00:00Z"), rc(head[:8], ""), "blocked: architect RC at aaaaaaaa"},
		{"lukas approved after: not the architect", &Facts{State: "OPEN", Oid: head, Reviews: []Review{{Login: Lukas, State: "APPROVED", At: "2026-11-12T12:00:00Z"}}}, rc(head[:8], ""), "blocked: architect RC at aaaaaaaa"},
		{"Ilya changes requested after", &Facts{State: "OPEN", Oid: head, Reviews: []Review{{Login: Ilya, State: "CHANGES_REQUESTED", At: "2026-11-12T12:00:00Z"}}}, rc(head[:8], ""), "blocked: architect RC at aaaaaaaa"},
		{"RC time unparseable: never superseded", ilya("2026-11-12T12:00:00Z"), &Arch{Verdict: "REQUEST_CHANGES", Sha: head[:8], At: "garbage"}, "blocked: architect RC at aaaaaaaa"},
		{"draft", &Facts{State: "OPEN", Draft: true, Oid: head}, rc(head[:8], ""), ""},
		{"merged", &Facts{State: "MERGED", Oid: head}, rc(head[:8], ""), ""},
		{"no verdict", open, nil, ""},
		{"approve", open, &Arch{Verdict: "APPROVE", Sha: head[:8]}, ""},
	} {
		if got := ArchBlock(tc.f, tc.a); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStateOfLifecycle(t *testing.T) {
	for _, tc := range []struct {
		f    *Facts
		want string
	}{
		{nil, "unknown"}, {&Facts{State: "MERGED"}, "merged"}, {&Facts{State: "CLOSED"}, "closed"},
		{&Facts{State: "OPEN", Draft: true, Mergeable: "CONFLICTING"}, "draft"},
		{&Facts{State: "OPEN", Mergeable: "CONFLICTING"}, "waiting: conflict"}, {&Facts{State: "OPEN"}, "unknown"},
	} {
		if got := StateOf(tc.f, nil); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}
