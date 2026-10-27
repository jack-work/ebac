package figaro

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLeaseTTLRenewsOnlyNearTheDeadline(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stamp := now.Add(-30 * 24 * time.Hour)
	for _, tc := range []struct {
		name      string
		ttl       time.Duration
		expiresAt time.Time
		due       bool
	}{
		{"no lifetime yet", 0, time.Time{}, true},
		{"renewed within the hour", 30*24*time.Hour + FormLease - 10*time.Minute, now.Add(FormLease - 10*time.Minute), false},
		{"two hours since renewal", 30*24*time.Hour + FormLease - 2*time.Hour, now.Add(FormLease - 2*time.Hour), true},
		{"already overdue", 29 * 24 * time.Hour, now.Add(-24 * time.Hour), true},
	} {
		var ttlMS, expMS int64
		if tc.ttl > 0 {
			ttlMS, expMS = tc.ttl.Milliseconds(), tc.expiresAt.UnixMilli()
			if got := time.UnixMilli(expMS - ttlMS); !got.Equal(stamp) && tc.name != "already overdue" {
				t.Fatalf("%s: fixture stamp %v, want %v", tc.name, got, stamp)
			}
		}
		ttl, due := leaseTTL(now, ttlMS, expMS)
		if due != tc.due {
			t.Errorf("%s: due=%v, want %v", tc.name, due, tc.due)
			continue
		}
		if !due {
			continue
		}
		start := now
		if ttlMS > 0 {
			start = time.UnixMilli(expMS - ttlMS)
		}
		deadline := start.Add(ttl)
		if d := deadline.Sub(now); d < FormLease || d > FormLease+2*time.Second {
			t.Errorf("%s: new deadline is now+%v, want now+%v", tc.name, d, FormLease)
		}
	}
}

func TestMintPatchSetsNameAndLeaseTogether(t *testing.T) {
	p := mintPatch("ebac-x")
	for key, want := range map[string]string{"name": `"ebac-x"`, systemTTLKey: `"168h0m0s"`} {
		e, ok := p.Entry(key)
		if !ok {
			t.Errorf("mint patch lacks %s", key)
			continue
		}
		if got := string(e.New); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}

func TestOnlyNotFoundMeansGone(t *testing.T) {
	for _, tc := range []struct {
		err  error
		gone bool
	}{
		{errors.New(`jsonrpc error -32000: attach @ab: aria @ab not found`), true},
		{errors.New(`dial @ab: unsupported transport scheme ""`), false},
		{errors.New(`dial unix /run/user/1000/figaro/angelus.sock: connect: connection refused`), false},
		{errors.New(`context deadline exceeded`), false},
	} {
		err := fmt.Errorf("read @ab: %w", Gone(tc.err))
		if got := errors.Is(err, ErrFormGone); got != tc.gone {
			t.Errorf("%q: gone=%v, want %v", tc.err, got, tc.gone)
		}
	}
}

func liveLease(t *testing.T, f *Figaro, id string) (ttlMS, expiresAt int64) {
	t.Helper()
	ang, err := f.Angelus()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ang.ListGlobal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Figaros {
		if r.ID == id {
			return r.TTLMS, r.ExpiresAt
		}
	}
	t.Fatalf("%s not listed", id)
	return 0, 0
}

func TestLiveMintedFormCarriesARenewableLease(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-lease")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })

	ttl, exp := liveLease(t, f, id)
	if ttl != FormLease.Milliseconds() {
		t.Fatalf("minted ttl %dms, want %dms", ttl, FormLease.Milliseconds())
	}
	if d := time.Until(time.UnixMilli(exp)); d < FormLease-time.Minute || d > FormLease {
		t.Fatalf("minted deadline now+%v", d)
	}
	form, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if form["name"] != "ebac-livetest-lease" {
		t.Errorf("name = %#v", form["name"])
	}
	if _, decoy := form[systemTTLKey]; decoy {
		t.Error("lease landed as a flat decoy key")
	}

	if renewed, err := f.RenewLease(ctx, id); err != nil || renewed {
		t.Fatalf("fresh lease renewed=%v err=%v; want a no-op", renewed, err)
	}
	if err := f.setLease(ctx, id, time.Hour); err != nil {
		t.Fatalf("shorten: %v", err)
	}
	if ttl, _ := liveLease(t, f, id); ttl != time.Hour.Milliseconds() {
		t.Fatalf("positive control: shortened ttl reads %dms", ttl)
	}
	if renewed, err := f.RenewLease(ctx, id); err != nil || !renewed {
		t.Fatalf("near-due lease renewed=%v err=%v", renewed, err)
	}
	_, exp = liveLease(t, f, id)
	if d := time.Until(time.UnixMilli(exp)); d < FormLease-time.Minute || d > FormLease+2*time.Second {
		t.Fatalf("renewed deadline now+%v, want now+%v", d, FormLease)
	}
}

func TestLiveRemovedFormIsGoneAndAnUnreachableDaemonIsNot(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-gone")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := NewFigaro("").Form(ctx, id); err != nil {
		t.Fatalf("positive control: fresh form unreadable: %v", err)
	}
	if err := f.FormRemove(ctx, id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	_, err = NewFigaro("").Form(ctx, id)
	if !errors.Is(err, ErrFormGone) {
		t.Errorf("removed form: %v; want ErrFormGone", err)
	}
	_, err = NewFigaro("/nonexistent/angelus.sock").Form(ctx, id)
	if err == nil || errors.Is(err, ErrFormGone) {
		t.Errorf("unreachable daemon: %v; must not read as gone", err)
	}
}
