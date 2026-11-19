package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/livetest"
	"github.com/gluck/ebac/internal/sched"
)

// RED if a default budget number moves: GHE GraphQL 40/h burst 10 floor 1000, REST 600/h burst 60; beat every minute.
func TestDaemonDefaultsAreTheStewardsNumbers(t *testing.T) {
	c := DefaultDaemonConfig().Budget.Credentials["github:microsoft.ghe.com"]
	if g := c["graphql"]; g.PerHour != 40 || g.Burst != 10 || g.Floor != 1000 {
		t.Fatalf("graphql %+v", g)
	}
	if r := c["rest"]; r.PerHour != 600 || r.Burst != 60 || r.Floor != 0 {
		t.Fatalf("rest %+v", r)
	}
	if BeatEvery != time.Minute {
		t.Fatalf("beat every %s", BeatEvery)
	}
}

func TestDaemonConfigFile(t *testing.T) {
	dir := t.TempDir()
	if c, err := loadDaemonConfig(filepath.Join(dir, "absent.json")); err != nil || c.Budget.Credentials["github:microsoft.ghe.com"]["graphql"].PerHour != 40 {
		t.Fatalf("absent file: %+v %v, want the defaults", c, err)
	}
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"budget":{"credentials":{"github:x":{"graphql":{"per_hour":7,"burst":2}}}}}`), 0o600)
	c, err := loadDaemonConfig(p)
	if err != nil || c.Budget.Credentials["github:x"]["graphql"].PerHour != 7 || len(c.Budget.Credentials) != 1 {
		t.Fatalf("file: %+v %v", c, err)
	}
	_ = os.WriteFile(p, []byte(`{"budget":`), 0o600)
	if _, err := loadDaemonConfig(p); err == nil {
		t.Fatal("a torn config was accepted")
	}
}

// Live: the first beat mints the heartbeat form (its kind set) and writes beat; the next reuses it; a reader sees
// beat.at and the critics. RED if writeBeat stops writing beat, or mints a second form.
func TestLiveBeatMintsOnceAndWrites(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	fig := figaro.NewFigaro("")
	kind := fmt.Sprintf("ebac.daemon-livetest-%d", time.Now().UnixNano())
	at := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	id, err := writeBeat(ctx, fig, kind, "ebac-livetest-beat", "", Beat{At: at, EveryS: 60, Critics: map[string]sched.Entry{"a": {Err: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	livetest.Remove(t, fig, id)
	id2, err := writeBeat(ctx, fig, kind, "ebac-livetest-beat", "", Beat{At: at.Add(time.Minute), EveryS: 60})
	if err != nil || id2 != id {
		t.Fatalf("second beat: %s %v, want the same form %s", id2, err, id)
	}
	f, err := fig.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(f["beat"])
	var got Beat
	_ = json.Unmarshal(b, &got)
	if f["kind"] != kind || !got.At.Equal(at.Add(time.Minute)) || got.EveryS != 60 {
		t.Fatalf("form kind %v beat %s", f["kind"], b)
	}
}
