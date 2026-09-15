package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live tests run against the real angelus. They are gated because they mint
// and kill real arias; run with:
//
//	EBAC_LIVE=1 go test -run Live ./...
//
// They exist because the two things the SDK conversion changed most are the
// two that cannot be unit-tested: whether a role-targeted send still reaches
// the holder, and whether a conditional write actually refuses a stale base.
// Both were previously "should work" and both are load-bearing.
func liveOrSkip(t *testing.T) *Figaro {
	t.Helper()
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	f := NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestLiveRoleSendReachesTheHolder(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()

	aria, err := f.NewAria(ctx, "sonnet")
	if err != nil {
		t.Fatalf("mint aria: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, aria) })

	role, err := f.FormNew(ctx, "ebac-livetest-role")
	if err != nil {
		t.Fatalf("mint role: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, role) })

	// Before the cast the role holds nobody, and Send must SAY so rather
	// than silently succeeding into the void.
	if err := f.Send(ctx, role, "should not arrive"); err == nil {
		t.Fatal("send to an unheld role reported success")
	} else if !strings.Contains(err.Error(), "target-aria") {
		t.Fatalf("unhelpful error for an unheld role: %v", err)
	}

	if err := f.Cast(ctx, aria, role); err != nil {
		t.Fatalf("cast: %v", err)
	}
	// The daemon refuses figaro.qua on a role's own endpoint, so this only
	// passes if Send resolved target-aria itself.
	if err := f.Send(ctx, role, "livetest: reply with exactly LIVEOK"); err != nil {
		t.Fatalf("send to a held role: %v", err)
	}
	t.Logf("role %s -> aria %s: delivered", role, aria)
}

func TestLiveConditionalWriteRefusesAStaleBase(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()

	id, err := f.FormNew(ctx, "ebac-livetest-cas")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })

	// Positive control on the primitive itself: a write at a version that
	// has already moved must be refused, or the retry loop above it is
	// decoration.
	c, err := f.node(ctx, id)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	before, err := c.Form(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := f.SetJSON(ctx, id, "a", 1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err = c.Set(ctx, patchOf("a", 2), before.Version)
	if err == nil {
		t.Fatal("a write against a stale version was ACCEPTED; there is no concurrency gate")
	}
	if !isVersionConflict(err) {
		t.Fatalf("conflict not recognised by isVersionConflict: %v", err)
	}
	t.Logf("stale write correctly refused: %v", err)
}

// The reason the gate exists: a poller and the reconciler write one form.
// Without version checking the loser's key vanished silently.
func TestLiveConcurrentWritersDoNotLoseKeys(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()

	id, err := f.FormNew(ctx, "ebac-livetest-race")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Separate connections, as separate processes would have.
			w := NewFigaro("")
			defer w.Close()
			errs[i] = w.SetJSON(ctx, id, "k"+string(rune('a'+i)), map[string]int{"i": i})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("writer %d: %v", i, e)
		}
	}

	time.Sleep(200 * time.Millisecond)
	form, err := f.Form(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	missing := []string{}
	for i := 0; i < writers; i++ {
		k := "k" + string(rune('a'+i))
		if _, ok := form[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d/%d concurrent writes were LOST: %v", len(missing), writers, missing)
	}
	t.Logf("%d concurrent writers, 0 lost keys", writers)
}
