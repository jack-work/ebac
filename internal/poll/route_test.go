package poll

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

func routedEv(id, route string, tier model.Tier) model.Event {
	return model.Event{Kind: model.EvCommentNew, Tier: tier, PR: "notes/captains-log", Comment: id, Route: route, Path: "p/" + id}
}

type sendLog struct {
	sent []string // target + " " + count line
	fail map[string]bool
}

func (l *sendLog) send(_ context.Context, to, msg string) error {
	if l.fail[to] {
		return errors.New("refused")
	}
	l.sent = append(l.sent, to+" "+msg)
	return nil
}

// routedPoller delivers in collect mode (one ping line per wake, no plugin
// brief), so the test sees exactly which role got how many events.
func routedPoller(l *sendLog, holders map[string]bool) *Poller {
	return &Poller{Out: io.Discard, Send: l.send,
		Holder: func(_ context.Context, to string) bool { return holders == nil || holders[to] }}
}

func routedCritic(evs ...model.Event) *model.Critic {
	w := &model.Critic{Name: "keel-captains-log", FormID: "@crit", Source: "keel-threads", PingEverySec: 60}
	w.Keel = &model.KeelRef{AlertRole: "@d407b690"}
	w.Runtime.PendingEvents = evs
	return w
}

func targets(l *sendLog) []string {
	var out []string
	for _, s := range l.sent {
		to, rest, _ := strings.Cut(s, " ")
		n := strings.Fields(strings.SplitN(rest, ": ", 2)[1])[0]
		out = append(out, to+"="+n)
	}
	sort.Strings(out)
	return out
}

func TestDeliverSendsOneWakePerRouteAndUnroutedToAlertTarget(t *testing.T) {
	l := &sendLog{}
	w := routedCritic(routedEv("1", "@6fa9788a", model.TierWake), routedEv("2", "", model.TierWake),
		routedEv("3", "@6fa9788a", model.TierWake), routedEv("4", "@2ea091e5", model.TierRecord))
	if err := routedPoller(l, nil).Deliver(context.Background(), w, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"@6fa9788a=2", "@d407b690=1"} // the record-only route wakes nobody
	if got := targets(l); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if len(w.Runtime.PendingEvents) != 0 || w.Runtime.RetryCount != 0 {
		t.Fatalf("pending %d retry %d, want 0 0", len(w.Runtime.PendingEvents), w.Runtime.RetryCount)
	}
}

func TestDeliverKeepsOnlyTheFailedRoutesEvents(t *testing.T) {
	l := &sendLog{fail: map[string]bool{"@6fa9788a": true}}
	w := routedCritic(routedEv("1", "@6fa9788a", model.TierWake), routedEv("2", "", model.TierWake))
	err := routedPoller(l, nil).Deliver(context.Background(), w, nil)
	if err == nil || !strings.Contains(err.Error(), "@6fa9788a: refused") {
		t.Fatalf("err %v, want the failed route named", err)
	}
	if got := targets(l); !reflect.DeepEqual(got, []string{"@d407b690=1"}) {
		t.Fatalf("sent %v, want only the alert target", got)
	}
	if len(w.Runtime.PendingEvents) != 1 || w.Runtime.PendingEvents[0].Comment != "1" || w.Runtime.RetryCount != 1 {
		t.Fatalf("pending %+v retry %d, want event 1 kept, retry 1", w.Runtime.PendingEvents, w.Runtime.RetryCount)
	}
	// The retry sends the kept route only: the delivered one is not re-sent.
	l.fail = nil
	w.Runtime.NextRetryAt = w.Runtime.NextRetryAt.AddDate(-1, 0, 0)
	w.Runtime.LastPing = w.Runtime.LastPing.AddDate(-1, 0, 0)
	if err := routedPoller(l, nil).Deliver(context.Background(), w, nil); err != nil {
		t.Fatal(err)
	}
	if got := targets(l); !reflect.DeepEqual(got, []string{"@6fa9788a=1", "@d407b690=1"}) {
		t.Fatalf("after retry sent %v", got)
	}
	if len(w.Runtime.PendingEvents) != 0 || w.Runtime.RetryCount != 0 {
		t.Fatalf("after retry pending %d retry %d", len(w.Runtime.PendingEvents), w.Runtime.RetryCount)
	}
}

func TestDeliverKeepsARouteWithNoHolderAndSendsTheRest(t *testing.T) {
	l := &sendLog{}
	w := routedCritic(routedEv("1", "@empty", model.TierWake), routedEv("2", "", model.TierWake))
	if err := routedPoller(l, map[string]bool{"@d407b690": true}).Deliver(context.Background(), w, nil); err != nil {
		t.Fatal(err)
	}
	if got := targets(l); !reflect.DeepEqual(got, []string{"@d407b690=1"}) {
		t.Fatalf("sent %v", got)
	}
	if len(w.Runtime.PendingEvents) != 1 || w.Runtime.PendingEvents[0].Route != "@empty" ||
		!strings.Contains(w.Runtime.LastError, "@empty") {
		t.Fatalf("pending %+v last error %q", w.Runtime.PendingEvents, w.Runtime.LastError)
	}
}

func TestClassifyRoutesByRoleNamesAndFallsBackWhenTheRosterIsUnreadable(t *testing.T) {
	set := `[{"role_in_path":true,"action":"keep"},{"default":true,"to":"@d407b690","action":"keep"}]`
	w := &model.Critic{Name: "k", FormID: "@crit", Source: "keel-threads", Trigger: json.RawMessage(set)}
	w.Runtime.TriggerErr = "" // the set is valid: Classify writes nothing
	ev := []model.Event{{Kind: model.EvCommentNew, Tier: model.TierWake, Path: "handoffs/figaro-steward.md"}}

	p := &Poller{Out: io.Discard, Roles: func(context.Context) (map[string]string, error) {
		return map[string]string{"figaro-steward": "@e44cf463"}, nil
	}}
	kept, _ := p.Classify(context.Background(), w, ev)
	if kept[0].Route != "@e44cf463" {
		t.Fatalf("route %q, want @e44cf463", kept[0].Route)
	}
	p.Roles = func(context.Context) (map[string]string, error) { return nil, errors.New("roster gone") }
	kept, _ = p.Classify(context.Background(), w, ev)
	if kept[0].Route != "@d407b690" {
		t.Fatalf("unreadable roster: route %q, want the default @d407b690", kept[0].Route)
	}
}

func TestParseRosterReadsRoleKeys(t *testing.T) {
	f := map[string]any{
		"role:ebac-steward":               "@6fa9788a -> cb63b3d4 scarf-length under figaro-steward",
		"role:captains-log-steward~moved": "@5a48ebbf -> 516d3b6b elves-pheasant",
		"role:a%2Eb":                      "@aaaa0001 -> x",
		"role:no-form":                    "deadbeef (not in the store)",
		"name":                            "ebac-roles",
	}
	want := map[string]string{"ebac-steward": "@6fa9788a", "a.b": "@aaaa0001"}
	if got := ParseRoster(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
