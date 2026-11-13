package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// Live: one ack is ONE form version carrying read.<critic> = {round, at, note}; the flat ack_* keys a seat left are
// removed by the first ack and never written again. Positive control: the old way (three flat SetJSON) is measured on
// the same form and costs three versions, so the instrument can tell one write from three.
func TestLiveAckIsOneWrite(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.FormNew(ctx, "ebac-livetest-ack")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	name := "livetest-ack-" + strings.TrimPrefix(id, "@")[:8]
	w := &model.Critic{Name: name, Mode: model.ModeTeams, Source: "teams-chat", FormID: id, EverySec: 60, CreatedAt: time.Now().UTC(),
		Chat: &model.ChatRef{ID: "48:notes", Topic: "t"}}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	version := func() uint64 {
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return cf.Version
	}
	// Control: the old three flat writes, as a seat on the old rule did.
	v0 := version()
	for k, v := range map[string]any{"ack_round": 1, "ack_at": "2026-11-13T00:00:00Z", "ack_note": "old"} {
		if err := f.SetJSON(ctx, id, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if d := version() - v0; d != 3 {
		t.Fatalf("control: three flat writes moved the version by %d; the instrument cannot count writes", d)
	}
	// First ack: the read key, plus the one-time removal of the three legacy keys.
	if err := cmdAck(ctx, globals{}, []string{"--critic", name, "--round", "7", "--note", "did it\nsecond line"}); err != nil {
		t.Fatal(err)
	}
	cf, _ := st.ReadForm(ctx, id)
	for _, k := range model.LegacyAckKeys {
		if _, ok := cf.Keys[k]; ok {
			t.Fatalf("legacy key %s survived the first ack", k)
		}
	}
	var read map[string]model.Read
	if err := json.Unmarshal(cf.Keys[model.ReadKey], &read); err != nil || read[name].Round != 7 || read[name].Note != "did it" || read[name].At == "" {
		t.Fatalf("read key: %s (%v)", cf.Keys[model.ReadKey], err)
	}
	// Steady state: one ack, one version.
	v1 := version()
	if err := cmdAck(ctx, globals{}, []string{"--critic", name, "--round", "8"}); err != nil {
		t.Fatal(err)
	}
	if d := version() - v1; d != 1 {
		t.Fatalf("an ack moved the version by %d; want 1", d)
	}
}
