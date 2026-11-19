package figaro

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// "unknown trunk": the daemon still lists a removed form whose log is gone
	// (xwal, 11-19: livetest-trigger-dcaa911b's @f95682bc); reading it can never succeed.
	return strings.Contains(s, "not found") || strings.Contains(s, "not in tree") || strings.Contains(s, "unknown trunk")
}

// gone marks a daemon not-found as ErrFormGone and passes anything else
// through unchanged.
func Gone(err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %v", ErrFormGone, err)
	}
	return err
}

// mintPatch names a new form. It names no system.* key: since figaro 0.74
// system.ttl is owner-held and ebac's uid is not the owner, so the lifetime
// goes through the broker (setLease) right after the mint.
func mintPatch(name string) message.Patch {
	nb, _ := json.Marshal(name)
	return form.Build(form.Snapshot{}, map[string]json.RawMessage{"name": nb}, nil)
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
	ctx, cancel := f.bound(ctx)
	defer cancel()
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
		return f.renewTo(ctx, id, ttl), nil
	}
	return false, fmt.Errorf("renew %s: %w", id, ErrFormGone)
}

// BrokerMaxTTL is the longest system.ttl the lifetime broker sets.
const BrokerMaxTTL = 30 * 24 * time.Hour

// overCap remembers the forms already said to be past the broker's cap: one line per form per process.
var overCap sync.Map

// renewTo asks the broker for the ttl a renewal needs, and whether it asked. The daemon measures ttl from the
// form's FIRST ttl write and an unset does not move that stamp (proved live 11-19), so a form older than
// BrokerMaxTTL-FormLease needs more than the broker sets (ebac-roster: 892h, refused every run). Such a form lives
// on figaro's keepalive (a form read daily stays): the renewal is skipped, said once per form.
func (f *Figaro) renewTo(ctx context.Context, id string, ttl time.Duration) bool {
	if ttl > BrokerMaxTTL {
		if _, said := overCap.LoadOrStore(id, true); !said {
			fmt.Fprintf(lifetimeLog, "ebac: lease %s: needs ttl %s, over the broker's %s (ttl counts from its first write): not renewed; it lives on keepalive\n", id, ttl.Round(time.Hour), BrokerMaxTTL)
		}
		return false
	}
	_ = f.setLease(ctx, id, ttl)
	return true
}

// LifetimeSock is the figaro-lifetime-broker's socket (interim for
// figaro#155): it sets system.ttl / system.keepalive on ebac's own forms as
// the owner, for uid ebac only. One JSON line each way.
var LifetimeSock = "/run/user/1000/figaro-lifetime.sock"

// lifetimeWarned makes a broker failure log once per process: without the
// broker a form simply keeps the lifetime it has, and ebac carries on.
var (
	lifetimeWarned atomic.Bool
	lifetimeLog    io.Writer = os.Stderr
)

// setLease sets the form's system.ttl through the lifetime broker. A broker
// that is absent or refuses is logged once per process and otherwise
// ignored: the lease is a week long precisely so that a bad hour costs
// nothing, and figaro's keepalive still holds a form that is read.
func (f *Figaro) setLease(ctx context.Context, id string, ttl time.Duration) error {
	v := ttl.String()
	if err := f.lifetime(ctx, id, systemTTLKey, &v); err != nil && !lifetimeWarned.Swap(true) {
		fmt.Fprintf(lifetimeLog, "ebac: lifetime broker %s: %v (further broker failures are not logged)\n", LifetimeSock, err)
	}
	return nil
}

// lifetime sets (or, with a nil value, unsets) a lifetime key through the broker.
func (f *Figaro) lifetime(ctx context.Context, id, key string, value *string) error {
	shown := "null"
	if value != nil {
		shown = *value
	}
	if err := f.broker(ctx, map[string]any{"form": id, "key": key, "value": value}); err != nil {
		return fmt.Errorf("%s %s=%s %w", id, key, shown, err)
	}
	return nil
}

// errBrokerRefused is the broker answering no (as opposed to no answer).
var errBrokerRefused = errors.New("refused")

// broker sends one request to the broker and reads its one-line answer.
func (f *Figaro) broker(ctx context.Context, req map[string]any) error {
	ctx, cancel := f.bound(ctx)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", LifetimeSock)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	ans, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(ans, &res); err != nil {
		return fmt.Errorf("answer %.200q: %w", ans, err)
	}
	if !res.OK {
		return fmt.Errorf("%w: %s", errBrokerRefused, res.Error)
	}
	return nil
}

// formGone is how BrokerKill re-checks a form before asking again (tests replace it).
var formGone = func(ctx context.Context, f *Figaro, id string) bool {
	_, err := f.Form(ctx, id)
	return errors.Is(err, ErrFormGone)
}

// BrokerKill removes one of ebac's own forms through the lifetime broker (op kill, MQ 19453; uid ebac holds no kill
// grant of its own). One form, never its children. Under load the answer can be late while the kill still applies,
// so a failed ask re-checks the form before asking once more: gone is done. A refusal is final.
func (f *Figaro) BrokerKill(ctx context.Context, id string) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = f.broker(ctx, map[string]any{"form": id, "op": "kill"}); err == nil || errors.Is(err, errBrokerRefused) {
			break
		}
		if formGone(ctx, f, id) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("kill %s: %w", id, err)
	}
	return nil
}

// renew keeps a form ebac is using alive. A failure is logged, never fatal:
// the lease is a week long precisely so that a bad hour costs nothing.
func Renew(ctx context.Context, fig *Figaro, id string) {
	if _, err := fig.RenewLease(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "ebac: lease %s: %v\n", id, err)
	}
}

// FormNewLease mints a form whose lifetime is lease: the mint names it, then
// the broker sets its system.ttl.
func (f *Figaro) FormNewLease(ctx context.Context, name string, lease time.Duration) (string, error) {
	id, err := f.formCreate(ctx, name)
	if err != nil {
		return "", err
	}
	_ = f.setLease(ctx, id, lease)
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
	lctx, cancel := f.bound(ctx)
	resp, err := AuditAng(rpc.MethodList, "", ang).ListGlobal(lctx)
	cancel()
	if err != nil {
		return fmt.Errorf("list for leases: %w", err)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	now := time.Now()
	for _, r := range resp.Figaros {
		if !want[r.ID] {
			continue
		}
		if ttl, due := leaseTTLFor(now, r.TTLMS, r.ExpiresAt, lease); due {
			f.renewTo(ctx, r.ID, ttl)
		}
	}
	return nil
}
