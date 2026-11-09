package rosterstate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func ms(d time.Duration) int64 { return now.Add(d).UnixMilli() }

// fixture is a small family: a leader seat holding two roles, its lieutenant
// (a fork of the leader's holder), a seat whose parent holds nothing, a role
// whose holder is gone, live and expired transient forks, and a fork that
// holds a role (a seat, not a transient).
func fixture() []model.Aria {
	return []model.Aria{
		{ID: "11111111aaaaaaaaaaaaaaaaaaaaaaaa", Handle: "lead-seat", Kind: "conversation", State: "idle", Model: "m1", ContextTokens: 250, ContextLimit: 1000, Parent: "root0000"},
		{ID: "22222222bbbbbbbbbbbbbbbbbbbbbbbb", Handle: "lt-seat", Kind: "conversation", State: "dormant", Model: "m1", ContextTokens: 990, ContextLimit: 1000, Parent: "11111111aaaaaaaaaaaaaaaaaaaaaaaa"},
		{ID: "33333333cccccccccccccccccccccccc", Handle: "lone-seat", Kind: "conversation", State: "active", Parent: "root0000"},
		{ID: "root0000dddddddddddddddddddddddd", Handle: "family-root", Kind: "conversation", State: "dormant"},
		{ID: "44444444eeeeeeeeeeeeeeeeeeeeeeee", Handle: "fork-live", Kind: "conversation", State: "idle", Parent: "11111111", TTLMS: 7200000, ExpiresAt: ms(time.Hour)},
		{ID: "55555555ffffffffffffffffffffffff", Handle: "fork-expired", Kind: "conversation", State: "dormant", Parent: "11111111", TTLMS: 3600000, ExpiresAt: ms(-time.Minute)},
		{ID: "66666666000000000000000000000000", Handle: "fork-of-loner", Kind: "conversation", State: "idle", Parent: "33333333cccccccccccccccccccccccc", TTLMS: 600000, ExpiresAt: ms(5 * time.Minute)},
		{ID: "77777777111111111111111111111111", Handle: "ttl-holder", Kind: "conversation", State: "idle", Parent: "11111111", TTLMS: 600000, ExpiresAt: ms(5 * time.Minute)},
		{ID: "@aaaa0001", Kind: "form", Name: "alpha-leader", TargetAria: "11111111"},
		{ID: "@aaaa0002", Kind: "form", Name: "alpha-steward", TargetAria: "11111111aaaaaaaaaaaaaaaaaaaaaaaa"},
		{ID: "@aaaa0003", Kind: "form", Name: "alpha-builder", TargetAria: "22222222"},
		{ID: "@aaaa0004", Kind: "form", Name: "ghost", TargetAria: "deadbeef"},
		{ID: "@aaaa0005", Kind: "form", Name: "loner.v2", TargetAria: "33333333"},
		{ID: "@aaaa0006", Kind: "form", Name: "short-lived", TargetAria: "77777777"},
		{ID: "@aaaa0007", Kind: "form", Name: "plain-form"}, // no target-aria: not a role
		{ID: "@aaaa0008", Kind: "form", Name: "ebac-pr-critic", TargetAria: "22222222"},
	}
}

func TestDeriveTheRoster(t *testing.T) {
	v := Derive(fixture(), now)
	want := map[string]string{
		"role:alpha-leader":   "@aaaa0001 -> 11111111 lead-seat m1 fill 20%",
		"role:alpha-steward":  "@aaaa0002 -> 11111111 lead-seat m1 fill 20%",
		"role:alpha-builder":  "@aaaa0003 -> 22222222 lt-seat m1 fill 90% asleep under alpha-leader/alpha-steward",
		"role:ghost":          "@aaaa0004 -> deadbeef (not in the store)",
		"role:loner%2Ev2":     "@aaaa0005 -> 33333333 lone-seat",
		"role:short-lived":    "@aaaa0006 -> 77777777 ttl-holder under alpha-leader/alpha-steward",
		"role:ebac-pr-critic": "@aaaa0008 -> 22222222 lt-seat m1 fill 90% asleep under alpha-leader/alpha-steward",
	}
	if len(v.Roles) != len(want) {
		t.Errorf("roles: got %d keys, want %d: %v", len(v.Roles), len(want), v.Roles)
	}
	for k, line := range want {
		if v.Roles[k] != line {
			t.Errorf("%s\n got  %q\n want %q", k, v.Roles[k], line)
		}
	}
	// Live transient forks only: not the expired one, not the one holding a
	// role; sorted by what they are under.
	wantForks := []string{
		"44444444 fork-live under alpha-leader/alpha-steward until 11-10 13:00Z",
		"66666666 fork-of-loner under loner.v2 until 11-10 12:05Z",
	}
	if strings.Join(v.Forks, "|") != strings.Join(wantForks, "|") {
		t.Errorf("forks\n got  %q\n want %q", v.Forks, wantForks)
	}
}

func TestDeriveCapsForks(t *testing.T) {
	arias := []model.Aria{{ID: "p0000000", Handle: "p", Kind: "conversation"}}
	for i := 0; i < ForksMax+3; i++ {
		arias = append(arias, model.Aria{ID: fmt.Sprintf("f%07d", i), Kind: "conversation", Parent: "p0000000", TTLMS: 1, ExpiresAt: ms(time.Hour)})
	}
	v := Derive(arias, now)
	if len(v.Forks) != ForksMax+1 || v.Forks[ForksMax] != "+3 more" {
		t.Fatalf("forks not capped: %d lines, last %q", len(v.Forks), v.Forks[len(v.Forks)-1])
	}
	if n := len(model.MustJSON(v.Forks)); n > 2046 {
		t.Fatalf("a capped forks value is %d bytes, over figaro's 2046 cut", n)
	}
}

// Writes only on change. Control: the first round and a changed role write;
// then an identical listing (even an hour later) writes nothing at all, not
// even updated.
func TestStepWritesOnlyChange(t *testing.T) {
	set, unset, st := Step(State{}, Derive(fixture(), now), now)
	if len(set) != 9 || len(unset) != 0 || set["updated"] == nil {
		t.Fatalf("first round: set %d keys (want 7 roles + forks + updated), unset %v", len(set), unset)
	}

	// The same store an hour later (Derive keeps its own clock: the forks'
	// expiry is fixed, so time passing changes nothing).
	later := now.Add(time.Hour - time.Minute)
	set, unset, _ = Step(st, Derive(fixture(), now), later)
	if len(set) != 0 || len(unset) != 0 {
		t.Fatalf("an unchanged listing wrote %v, unset %v", keysOf(set), unset)
	}

	moved := fixture()
	moved[0].ContextTokens = 500 // lead-seat 20% -> 50%: its two roles change
	moved = moved[:len(moved)-1] // the pr critic role is gone
	set, unset, _ = Step(st, Derive(moved, now), later)
	if got := keysOf(set); got != "role:alpha-leader,role:alpha-steward,updated" {
		t.Fatalf("changed keys: %s", got)
	}
	if strings.Join(unset, ",") != "role:ebac-pr-critic" {
		t.Fatalf("unset: %v", unset)
	}
}

func keysOf(m map[string]json.RawMessage) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// Through the protocol: state rides in the critic's runtime, so the second
// round with the same store writes nothing; the plugin refuses an empty
// store (a core that read nothing) and every op but round.
func TestPluginRounds(t *testing.T) {
	ip := plugin.InProc{"roster": Plugin{Now: func() time.Time { return now }}.Handle}
	c := model.Critic{Name: "roles", Source: model.SourceRoster}
	rep, err := ip.Call(context.Background(), "roster", plugin.Request{Op: plugin.OpRound, Critic: c, Arias: fixture()})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || len(rep.Events) != 0 || len(rep.Set) != 9 {
		t.Fatalf("round 1: done %v events %d keys %d", rep.Done, len(rep.Events), len(rep.Set))
	}
	c.Runtime = rep.Critic.Runtime
	rep, err = ip.Call(context.Background(), "roster", plugin.Request{Op: plugin.OpRound, Critic: c, Arias: fixture()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Set) != 0 || len(rep.Unset) != 0 || !strings.Contains(strings.Join(rep.Log, ""), "unchanged") {
		t.Fatalf("round 2 on the same store wrote %v / unset %v (%v)", keysOf(rep.Set), rep.Unset, rep.Log)
	}
	if _, err := ip.Call(context.Background(), "roster", plugin.Request{Op: plugin.OpRound, Critic: c}); err == nil || !strings.Contains(err.Error(), "the core passed no arias") {
		t.Fatalf("empty store: %v", err)
	}
	if _, err := ip.Call(context.Background(), "roster", plugin.Request{Op: plugin.OpBrief, Critic: c}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("brief: %v", err)
	}
}
