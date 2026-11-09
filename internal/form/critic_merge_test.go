package form

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func TestMergeCritic(t *testing.T) {
	base := `{"name":"c","write":false,"every_sec":60,"prs":["a"]}`
	for _, c := range []struct {
		name, mine, theirs, want, conflicts string
	}{
		{"no race: the round's change lands", `{"name":"c","write":false,"every_sec":30,"prs":["a"]}`, base, `{"every_sec":30,"name":"c","prs":["a"],"write":false}`, ""},
		{"a live set survives an untouched field", base, `{"name":"c","write":true,"every_sec":60,"prs":["a"]}`, `{"every_sec":60,"name":"c","prs":["a"],"write":true}`, ""},
		{"both sides, different fields", `{"name":"c","write":false,"every_sec":30,"prs":["a"]}`, `{"name":"c","write":true,"every_sec":60,"prs":["a"]}`, `{"every_sec":30,"name":"c","prs":["a"],"write":true}`, ""},
		{"same field, different values: the form wins, reported", `{"name":"c","write":false,"every_sec":30,"prs":["a"]}`, `{"name":"c","write":false,"every_sec":99,"prs":["a"]}`, `{"every_sec":99,"name":"c","prs":["a"],"write":false}`, "every_sec"},
		{"same field, same value: no conflict", `{"name":"c","write":true,"every_sec":60,"prs":["a"]}`, `{"name":"c","write":true,"every_sec":60,"prs":["a"]}`, `{"every_sec":60,"name":"c","prs":["a"],"write":true}`, ""},
		{"a field the round dropped (omitempty) goes", `{"name":"c","every_sec":60,"prs":["a"]}`, base, `{"every_sec":60,"name":"c","prs":["a"]}`, ""},
		{"a field the form added stays", base, `{"name":"c","write":false,"every_sec":60,"prs":["a"],"approve":true}`, `{"approve":true,"every_sec":60,"name":"c","prs":["a"],"write":false}`, ""},
		{"another layout of the same value is no change", base, `{ "prs": [ "a" ], "every_sec": 60, "write": false, "name": "c" }`, `{"every_sec":60,"name":"c","prs":["a"],"write":false}`, ""},
	} {
		got, conf, err := mergeCritic(json.RawMessage(base), json.RawMessage(c.mine), json.RawMessage(c.theirs))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want || strings.Join(conf, ",") != c.conflicts {
			t.Errorf("%s:\n got %s conflicts %v\nwant %s conflicts %q", c.name, got, conf, c.want, c.conflicts)
		}
	}
	// No base (a critic this process created): ours, whole.
	if got, _, _ := mergeCritic(nil, json.RawMessage(`{"name":"new"}`), json.RawMessage(base)); string(got) != `{"name":"new"}` {
		t.Fatalf("no base: %s", got)
	}
}

// Live, through the daemon: a `figaro set critic.write true` that lands between a round's load and its save survives
// the save, beside a control with no race, and a same-field race that keeps the form's value and says so.
func TestLiveRacingCriticSetSurvivesSave(t *testing.T) {
	f := liveOrSkip(t)
	if _, err := exec.LookPath("figaro"); err != nil {
		t.Skip("the race is the figaro CLI's dotted set; no figaro on PATH")
	}
	ctx := context.Background()
	st, err := NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	var warn strings.Builder
	st.Warn = &warn
	id, err := f.FormNew(ctx, "ebac-livetest-cas")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	name := "livetest-cas-" + strings.TrimPrefix(id, "@")[:8]
	w := &model.Critic{Name: name, FormID: id, Source: "keel-threads", CreatedAt: time.Now().UTC(), EverySec: 60,
		Keel: &model.KeelRef{Repo: "notes/x"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	critic := func() map[string]any {
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, flat := cf.Keys["critic.write"]; flat {
			t.Fatal("the dotted set landed as a top-level key, not inside critic: the race is not the real one")
		}
		var m map[string]any
		if err := json.Unmarshal(cf.Keys["critic"], &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	round := func(every int, race func()) *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if race != nil {
			race()
		}
		c.EverySec = every
		c.Runtime.Round++
		if err := st.SaveCritic(ctx, c); err != nil {
			t.Fatalf("save: %v", err)
		}
		return c
	}

	// Control: no race. The round's change lands, nothing else moves.
	round(45, nil)
	if m := critic(); m["every_sec"] != float64(45) || m["write"] != nil {
		t.Fatalf("control: critic %v", m)
	}

	// The race: the operator grants write between load and save.
	round(30, func() {
		figaroSet(t, id, "critic.write", "true")
		if critic()["write"] != true {
			t.Fatal("the racing set did not land on the form")
		}
	})
	m := critic()
	if m["write"] != true {
		t.Fatalf("the racing critic.write was overwritten by the save: %v", m)
	}
	if m["every_sec"] != float64(30) {
		t.Fatalf("the round's own change was lost: %v", m)
	}
	if c, _ := st.LoadCritic(ctx, name); c.Runtime.Round != 2 || !c.Write {
		t.Fatalf("after the race: round %d write %v", c.Runtime.Round, c.Write)
	}
	if warn.Len() != 0 {
		t.Fatalf("a different-field race warned: %s", warn.String())
	}

	// Same field: the form's value wins, and the conflict is reported with its field.
	round(15, func() {
		figaroSet(t, id, "critic.every_sec", "99")
	})
	if m := critic(); m["every_sec"] != float64(99) || m["write"] != true {
		t.Fatalf("same-field race: %v", m)
	}
	if !strings.Contains(warn.String(), "every_sec changed on the form during the round") {
		t.Fatalf("conflict not reported: %q", warn.String())
	}
}

// figaroSet is the operator's gesture exactly: the CLI's dotted set into the critic subtree.
func figaroSet(t *testing.T, id, key, value string) {
	t.Helper()
	if out, err := exec.Command("figaro", "set", "--id", id, key, value).CombinedOutput(); err != nil {
		t.Fatalf("figaro set %s %s: %v\n%s", key, value, err, out)
	}
}
