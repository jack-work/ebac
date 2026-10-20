package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jack-work/figaro/api/form"
	"github.com/jack-work/figaro/api/message"
)

// Every form ebac mints carries figaro's native lifetime (system.ttl) and is
// renewed by the process that uses it. Debris therefore expires on its own and
// no rule ever has to decide what to delete.
//
// FormLease must outlast any outage in which renewals fail: on 2026-10-19 they
// failed for 2h18m, and a lease that short would have reaped live roles.
const (
	FormLease = 7 * 24 * time.Hour
	// leaseRenewSlack bounds renewal writes to about one per hour per form;
	// each write moves the form's version.
	leaseRenewSlack = time.Hour
	systemTTLKey    = "system.ttl"
)

// ErrFormGone is the daemon saying a form does not exist. Only this justifies
// minting a replacement; any other read failure says nothing about the form.
var ErrFormGone = errors.New("form gone")

// isNotFound matches the daemon's wording, as figaro's own CLI does: the
// refusal carries no distinct code.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "not found") || strings.Contains(s, "not in tree")
}

// gone marks a daemon not-found as ErrFormGone and passes anything else
// through unchanged.
func gone(err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %v", ErrFormGone, err)
	}
	return err
}

// mintPatch names a new form and sets its lifetime in the same write, so no
// form ebac creates is ever without one.
func mintPatch(name string) message.Patch {
	nb, _ := json.Marshal(name)
	tb, _ := json.Marshal(FormLease.String())
	return form.Build(form.Snapshot{}, map[string]json.RawMessage{"name": nb, systemTTLKey: tb}, nil)
}

// leaseTTL returns the system.ttl that puts the deadline FormLease from now,
// and whether the current deadline is near enough to be worth a write. The
// daemon measures ttl from a stamp it took at the first ttl write, which it
// reports only as expiresAt-ttl.
func leaseTTL(now time.Time, ttlMS, expiresAtMS int64) (time.Duration, bool) {
	if ttlMS <= 0 {
		return FormLease, true
	}
	if time.UnixMilli(expiresAtMS).Sub(now) > FormLease-leaseRenewSlack {
		return 0, false
	}
	stamp := time.UnixMilli(expiresAtMS - ttlMS)
	return (now.Sub(stamp) + FormLease).Truncate(time.Second) + time.Second, true
}

// RenewLease moves a form's deadline to FormLease from now when it has come
// within leaseRenewSlack of needing it. ErrFormGone if the form does not exist.
func (f *Figaro) RenewLease(ctx context.Context, id string) (bool, error) {
	ang, err := f.angelus()
	if err != nil {
		return false, err
	}
	resp, err := ang.ListGlobal(ctx)
	if err != nil {
		return false, fmt.Errorf("list for lease: %w", err)
	}
	for _, r := range resp.Figaros {
		if r.ID != id {
			continue
		}
		ttl, due := leaseTTL(time.Now(), r.TTLMS, r.ExpiresAt)
		if !due {
			return false, nil
		}
		return true, f.setLease(ctx, id, ttl)
	}
	return false, fmt.Errorf("renew %s: %w", id, ErrFormGone)
}

// setLease writes system.ttl through figaro's own path builder: ebac's flat
// SetJSON refuses dotted keys, and a flat "system.ttl" key would be a decoy.
func (f *Figaro) setLease(ctx context.Context, id string, ttl time.Duration) error {
	c, err := f.node(ctx, id)
	if err != nil {
		return err
	}
	tb, _ := json.Marshal(ttl.String())
	var lastErr error
	for i := 0; i < 4; i++ {
		cur, err := c.Form(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", id, err)
		}
		patch := form.Build(cur.Snapshot, map[string]json.RawMessage{systemTTLKey: tb}, nil)
		if _, lastErr = c.Set(ctx, patch, cur.Version); lastErr == nil || !isVersionConflict(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// renew keeps a form ebac is using alive. A failure is logged, never fatal:
// the lease is a week long precisely so that a bad hour costs nothing.
func renew(ctx context.Context, fig *Figaro, id string) {
	if _, err := fig.RenewLease(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "ebac: lease %s: %v\n", id, err)
	}
}
