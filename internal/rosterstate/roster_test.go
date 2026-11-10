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
		"role:alpha-leader":   "@aaaa0001 -> 11111111 lead-seat",
		"role:alpha-steward":  "@aaaa0002 -> 11111111 lead-seat",
		"role:alpha-builder":  "@aaaa0003 -> 22222222 lt-seat under alpha-leader/alpha-steward",
		"role:ghost":          "@aaaa0004 -> deadbeef (not in the store)",
		"role:loner%2Ev2":     "@aaaa0005 -> 33333333 lone-seat",
		"role:short-lived":    "@aaaa0006 -> 77777777 ttl-holder under alpha-leader/alpha-steward",
		"role:ebac-pr-critic": "@aaaa0008 -> 22222222 lt-seat under alpha-leader/alpha-steward",
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
	// role; newest first (fork-of-loner minted at 11:55, fork-live at 11:00).
	wantForks := []string{
		"66666666 fork-of-loner under loner.v2",
		"44444444 fork-live under alpha-leader/alpha-steward",
	}
	if strings.Join(v.Forks, "|") != strings.Join(wantForks, "|") {
		t.Errorf("forks\n got  %q\n want %q", v.Forks, wantForks)
	}
}

func TestDeriveCapsForks(t *testing.T) {
	arias := []model.Aria{{ID: "p0000000", Handle: "p", Kind: "conversation"}}
	for i := 0; i < ForksMax+3; i++ { // f0000000 minted first, each a minute after the last
		arias = append(arias, model.Aria{ID: fmt.Sprintf("f%07d", i), Kind: "conversation", Parent: "p0000000", TTLMS: 3600000, ExpiresAt: ms(time.Hour + time.Duration(i)*time.Minute)})
	}
	v := Derive(arias, now)
	if len(v.Forks) != ForksMax+1 || v.Forks[ForksMax] != "+3 more (figaro ls)" {
		t.Fatalf("forks not capped: %d lines, last %q", len(v.Forks), v.Forks[len(v.Forks)-1])
	}
	if !strings.HasPrefix(v.Forks[0], fmt.Sprintf("f%07d ", ForksMax+2)) || !strings.HasPrefix(v.Forks[ForksMax-1], "f0000003 ") {
		t.Fatalf("not newest first: first %q, last kept %q", v.Forks[0], v.Forks[ForksMax-1])
	}
	if n := len(model.MustJSON(v.Forks)); n > 2046 {
		t.Fatalf("a capped forks value is %d bytes, over figaro's 2046 cut", n)
	}
}

// Writes only when the set of roles, holders or forks changes. The first round
// writes every key and unsets the dropped updated once; then a store whose seats
// merely WORKED (context fill, asleep/awake, model, minutes on the clock) writes
// nothing; a new fork writes only forks; a new holder only its role; a gone role
// is unset.
func TestStepWritesOnlyChange(t *testing.T) {
	set, unset, st := Step(State{}, Derive(fixture(), now))
	if len(set) != 8 || set["updated"] != nil || strings.Join(unset, ",") != "updated" {
		t.Fatalf("first round: set %d keys (want 7 roles + forks), unset %v", len(set), unset)
	}

	later := now.Add(4 * time.Minute) // before the first fork expires: an expiry is a real change
	worked := fixture()
	for i := range worked {
		worked[i].ContextTokens += 400
		worked[i].Model = "m2"
		if worked[i].State == "dormant" {
			worked[i].State = "active"
		} else if worked[i].Kind == "conversation" {
			worked[i].State = "dormant"
		}
	}
	set, unset, st2 := Step(st, Derive(worked, later))
	if len(set) != 0 || len(unset) != 0 {
		t.Fatalf("an unchanged set of seats wrote %v, unset %v", keysOf(set), unset)
	}
	set, unset, _ = Step(st2, Derive(fixture(), later))
	if len(set) != 0 || len(unset) != 0 {
		t.Fatalf("a third identical round wrote %v, unset %v", keysOf(set), unset)
	}

	forked := append(fixture(), model.Aria{ID: "88888888222222222222222222222222", Handle: "fork-new", Kind: "conversation",
		Parent: "22222222", TTLMS: 600000, ExpiresAt: ms(9 * time.Minute)})
	set, unset, _ = Step(st, Derive(forked, now))
	if keysOf(set) != "forks" || len(unset) != 0 || !strings.HasPrefix(string(set["forks"]), `["88888888 fork-new under alpha-builder/ebac-pr-critic"`) {
		t.Fatalf("a new fork: set %s %s, unset %v", keysOf(set), set["forks"], unset)
	}

	moved := fixture()
	moved[len(moved)-6].TargetAria = "77777777" // alpha-builder -> ttl-holder
	moved = moved[:len(moved)-1]                // the pr critic role is gone
	set, unset, _ = Step(st, Derive(moved, now))
	if got := keysOf(set); got != "role:alpha-builder" {
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
	if !rep.Done || len(rep.Events) != 0 || len(rep.Set) != 8 || strings.Join(rep.Unset, ",") != "updated" {
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
