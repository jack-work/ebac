package figaro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/jack-work/figaro/api/form"
	"github.com/jack-work/figaro/api/message"
	"github.com/jack-work/figaro/api/rpc"
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
func Gone(err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %v", ErrFormGone, err)
	}
	return err
}

// mintPatch names a new form and sets its lifetime in the same write, so no
// form ebac creates is ever without one.
func mintPatch(name string) message.Patch {
	nb, _ := json.Marshal(name)
	sys := model.MustJSON(map[string]string{"ttl": FormLease.String()})
	return form.Build(form.Snapshot{}, map[string]json.RawMessage{"name": nb, "system": sys}, nil)
}

// leaseTTL returns the system.ttl that puts the deadline FormLease from now,
// and whether the current deadline is near enough to be worth a write. The
// daemon measures ttl from a stamp it took at the first ttl write, which it
// reports only as expiresAt-ttl.
func leaseTTL(now time.Time, ttlMS, expiresAtMS int64) (time.Duration, bool) {
	return leaseTTLFor(now, ttlMS, expiresAtMS, FormLease)
}

// leaseTTLFor is leaseTTL for any lease length.
func leaseTTLFor(now time.Time, ttlMS, expiresAtMS int64, lease time.Duration) (time.Duration, bool) {
	if ttlMS <= 0 {
		return lease, true
	}
	if time.UnixMilli(expiresAtMS).Sub(now) > lease-leaseRenewSlack {
		return 0, false
	}
	stamp := time.UnixMilli(expiresAtMS - ttlMS)
	return (now.Sub(stamp) + lease).Truncate(time.Second) + time.Second, true
}

// RenewLease moves a form's deadline to FormLease from now when it has come
// within leaseRenewSlack of needing it. ErrFormGone if the form does not exist.
func (f *Figaro) RenewLease(ctx context.Context, id string) (bool, error) {
	ang, err := f.Angelus()
	if err != nil {
		return false, err
	}
	resp, err := AuditAng(rpc.MethodList, "", ang).ListGlobal(ctx)
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

// setLease merges ttl into the system subtree and writes it whole: a nested
// SetPath can land as a flat decoy when system is a leaf (jack-work/figaro#43).
func (f *Figaro) setLease(ctx context.Context, id string, ttl time.Duration) error {
	c, err := f.Node(ctx, id)
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
		raw, _ := cur.Snapshot.Get("system")
		system := model.ObjectOf(raw)
		system["ttl"] = tb
		patch := cur.Snapshot.SetPath("system", model.MustJSON(system)).Diff(cur.Snapshot)
		if _, lastErr = c.Set(ctx, patch, cur.Version); lastErr == nil || !IsVersionConflict(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// renew keeps a form ebac is using alive. A failure is logged, never fatal:
// the lease is a week long precisely so that a bad hour costs nothing.
func Renew(ctx context.Context, fig *Figaro, id string) {
	if _, err := fig.RenewLease(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "ebac: lease %s: %v\n", id, err)
	}
}

// FormNewLease mints a form whose lifetime is lease (FormNew uses FormLease).
func (f *Figaro) FormNewLease(ctx context.Context, name string, lease time.Duration) (string, error) {
	id, err := f.FormNew(ctx, name)
	if err != nil {
		return "", err
	}
	if lease != FormLease {
		if err := f.setLease(ctx, id, lease); err != nil {
			return id, fmt.Errorf("lease %s: %w", id, err)
		}
	}
	return id, nil
}

// RenewLeases keeps each form's deadline lease from now, writing only the
// ones within leaseRenewSlack of needing it, with ONE list of the store for
// all of them (RenewLease lists per form). Forms the store no longer has are
// skipped; the first write error is returned after trying the rest.
func (f *Figaro) RenewLeases(ctx context.Context, ids []string, lease time.Duration) error {
	ang, err := f.Angelus()
	if err != nil {
		return err
	}
	resp, err := AuditAng(rpc.MethodList, "", ang).ListGlobal(ctx)
	if err != nil {
		return fmt.Errorf("list for leases: %w", err)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var first error
	now := time.Now()
	for _, r := range resp.Figaros {
		if !want[r.ID] {
			continue
		}
		ttl, due := leaseTTLFor(now, r.TTLMS, r.ExpiresAt, lease)
		if !due {
			continue
		}
		if err := f.setLease(ctx, r.ID, ttl); err != nil && first == nil {
			first = fmt.Errorf("renew %s: %w", r.ID, err)
		}
	}
	return first
}
