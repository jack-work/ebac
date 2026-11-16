package figaro

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"time"
)

// CallTimeout bounds every call ebac makes to the figaro daemon (figaro#115).
// A healthy daemon answers a full list in well under a second; at the
// 2026-11-16 05:00Z bounce, with no deadline, one reconcile waited 1121 s on
// figaro.list and twelve polls piled up behind the same socket.
const CallTimeout = 60 * time.Second

// bound gives one call its deadline: f.Timeout from now, or the caller's own
// deadline if that is sooner. Composite methods (Form, Send, RenewLeases)
// bound each call they make rather than the whole sequence.
func (f *Figaro) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, f.Timeout)
}

// Unavailable reports a failure to REACH the daemon, as opposed to the
// daemon answering with an error: a call that hit its deadline, a refused or
// missing socket, a full backlog, or a connection that closed under the call.
// These are the failures a caller backs off from. The SDK does not wrap every
// error, so the message is checked after errors.Is.
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	m := err.Error()
	for _, s := range []string{"context deadline exceeded", "connection refused", "dial angelus at",
		"connection closed", "broken pipe", "connection reset"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
