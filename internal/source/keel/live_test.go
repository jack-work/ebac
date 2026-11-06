package keel

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
	"github.com/gluck/ebac/internal/poll"
)

func init() { poll.Register(Source{}) }

// Live tests run against the real angelus (they mint and remove a form):
//
//	EBAC_LIVE=1 go test -run Live ./...
func liveOrSkip(t *testing.T) *figaro.Figaro {
	t.Helper()
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// One keel critic through baseline, a Boss answer, blind and gone, against a
// fake door on a unix socket.
func TestLiveKeelRoundsBaselineAnswerBlindGone(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	d := newDoor(t)
	t.Setenv("EBAC_KEEL_SOCKET", d.sock)
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-keel")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	w := &model.Critic{Name: "livetest-keel-" + strings.TrimPrefix(id, "@")[:8], Mode: model.ModeKeel, Source: "keel-threads",
		CreatedAt: time.Now().UTC(), FormID: id, Keel: ref()}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	round := func() error {
		return (&poll.Poller{Store: st, Fig: f, Out: &out}).Poll(ctx, w.Name)
	}
	load := func() *model.Critic {
		c, err := st.LoadCritic(ctx, w.Name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	key := func(k string) (string, bool) {
		fm, err := f.Form(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := fm[k]
		b, _ := json.Marshal(v)
		return string(b), ok
	}

	q := th("t1", false, cm("c1", "figaro-seat", "question?"))
	d.set(200, payload(map[string][]Thread{"doc/a.md": {q}}))
	if err := round(); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if c := load(); len(c.Runtime.Keel.Seen) != 1 || len(c.Runtime.PendingEvents) != 0 {
		t.Fatalf("baseline: %+v pending %v", c.Runtime.Keel, c.Runtime.PendingEvents)
	}
	if ix, ok := key("threads"); !ok || !strings.Contains(ix, `"t1"`) {
		t.Fatalf("no thread index: %s", ix)
	}

	// The Boss answers with nobody seated: recorded, inbox written, delivery refused loudly.
	q.Comments = append(q.Comments, cm("c2", boss, "please look"))
	d.set(200, payload(map[string][]Thread{"doc/a.md": {q}}))
	if err := round(); err != nil {
		t.Fatalf("answer: %v", err)
	}
	c := load()
	if len(c.Runtime.PendingEvents) != 1 || c.Runtime.PendingEvents[0].Tier != model.TierWake || !strings.Contains(c.Runtime.LastError, "no reachable target-aria") {
		t.Fatalf("answer: pending %+v lastError %q", c.Runtime.PendingEvents, c.Runtime.LastError)
	}
	if inbox, _ := key("inbox"); !strings.Contains(inbox, "please look") {
		t.Fatalf("inbox lacks the answer: %s", inbox)
	}

	// Blind: a young failure writes nothing (control); past the threshold it marks; recovery clears.
	d.set(500, "boom")
	_ = round()
	if _, ok := key("blind"); ok {
		t.Fatal("one failed round already marked blind")
	}
	orig := keelBlindAfter
	keelBlindAfter = 0
	t.Cleanup(func() { keelBlindAfter = orig })
	_ = round()
	if _, ok := key("blind"); !ok || !load().Runtime.Keel.Blind {
		t.Fatal("not blind after the threshold")
	}
	d.set(200, payload(map[string][]Thread{"doc/a.md": {q}}))
	if err := round(); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if _, ok := key("blind"); ok || load().Runtime.Keel.Blind {
		t.Fatal("blind not cleared on recovery")
	}
	keelBlindAfter = orig

	// Gone: a 500 never counts; two 404s then a success reset; three 404s stop it.
	d.set(404, "not found")
	_ = round()
	_ = round()
	d.set(500, "boom")
	_ = round()
	if c := load(); c.Runtime.Keel.GoneStreak != 0 || c.Runtime.Stopped {
		t.Fatalf("after 2x404+500: streak %d stopped %v", c.Runtime.Keel.GoneStreak, c.Runtime.Stopped)
	}
	d.set(404, "not found")
	for i := 0; i < keelGoneAfter; i++ {
		_ = round()
	}
	c = load()
	if !c.Runtime.Stopped || !strings.Contains(c.Runtime.StopReason, "keel threads gone") {
		t.Fatalf("not stopped after %d gones: %+v", keelGoneAfter, c.Runtime)
	}
	if gone, _ := key("gone"); !strings.Contains(gone, "jack-work/notes") {
		t.Fatalf("gone marker missing: %s", gone)
	}
	if err := round(); err != nil || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("a stopped critic polled again: %v\n%s", err, out.String())
	}
}
