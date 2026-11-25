// Package callpace paces the GraphQL calls of one-shot ebac verbs run by
// seats (the operator wrapper's --caller calls) against one budget shared by
// every such process: at most Limit points in any Window. A call over budget
// WAITS for the window to free; it never fails (#76: a scrub batch of 75
// comment-edit calls spent 156 points of the shared pool in 4 minutes).
//
// The budget is a small JSON file under ebac's state root, read and written
// under flock, so concurrent verbs share it.
package callpace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	// Limit is the most points the paced calls may spend in one Window.
	Limit = 30
	// Window is the sliding window Limit holds over.
	Window = 5 * time.Minute
	// FileName is the budget file under ebac's state root.
	FileName = "call-pace.json"
)

// Ledger is one process's view of the shared budget. It implements
// forge.Pacer.
type Ledger struct {
	Path   string
	Limit  int
	Window time.Duration
	Now    func() time.Time
	// Sleep waits d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Note hears one line per wait, and a budget file it could not use.
	Note io.Writer
	// Booked, if set, hears each reservation's time, under the budget's lock
	// (a test reads the shared schedule from it).
	Booked func(at time.Time)

	mu   sync.Mutex
	seq  int
	last string // the id of this process's newest reservation, for Settle
}

// New is the ledger at root/FileName with the package's limit and window.
func New(root string, note io.Writer) *Ledger {
	return &Ledger{Path: filepath.Join(root, FileName), Limit: Limit, Window: Window, Note: note}
}

type entry struct {
	At  time.Time `json:"at"`
	Pts int       `json:"pts"`
	ID  string    `json:"id"`
}

type book struct {
	Calls []entry `json:"calls"`
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Ledger) sleep(ctx context.Context, d time.Duration) error {
	if l.Sleep != nil {
		return l.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *Ledger) note(f string, a ...any) {
	if l.Note != nil {
		fmt.Fprintf(l.Note, "ebac: call pace: "+f+"\n", a...)
	}
}

// Wait blocks until one more point fits the window, then reserves it.
func (l *Ledger) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait, used, err := l.reserve()
		if err != nil {
			// A budget file this process cannot use paces nothing; the call
			// goes ahead (wait, never fail) and says so.
			l.note("budget %s unusable, not paced: %v", l.Path, err)
			return nil
		}
		if wait <= 0 {
			return nil
		}
		l.note("%d of %d points spent in the last %s; waiting %s", used, l.Limit, l.Window, wait.Round(time.Second))
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Settle books extra points on this process's newest reservation.
func (l *Ledger) Settle(extra int) {
	if extra <= 0 {
		return
	}
	l.mu.Lock()
	id := l.last
	l.mu.Unlock()
	if err := l.update(func(b *book, now time.Time) {
		for i := range b.Calls {
			if b.Calls[i].ID == id {
				b.Calls[i].Pts += extra
				return
			}
		}
		b.Calls = append(b.Calls, entry{At: now, Pts: extra, ID: id})
	}); err != nil {
		l.note("budget %s unusable, %d point(s) not booked: %v", l.Path, extra, err)
	}
}

// reserve books one point now if it fits, else says how long until it does.
func (l *Ledger) reserve() (wait time.Duration, used int, err error) {
	err = l.update(func(b *book, now time.Time) {
		used = 0
		for _, e := range b.Calls {
			used += e.Pts
		}
		if used+1 <= l.Limit {
			l.mu.Lock()
			l.seq++
			id := fmt.Sprintf("%d.%d", os.Getpid(), l.seq)
			l.last = id
			l.mu.Unlock()
			b.Calls = append(b.Calls, entry{At: now, Pts: 1, ID: id})
			if l.Booked != nil {
				l.Booked(now)
			}
			wait = 0
			return
		}
		// The oldest entries leave the window first: wait until enough of
		// them have left for one more point.
		over := used + 1 - l.Limit
		for _, e := range b.Calls {
			over -= e.Pts
			if over <= 0 {
				wait = e.At.Add(l.Window).Sub(now)
				if wait <= 0 {
					wait = time.Millisecond
				}
				return
			}
		}
		wait = l.Window // a single entry over the limit: one whole window
	})
	return wait, used, err
}

// update runs f on the book, pruned to the window and oldest first, under an
// exclusive lock, and writes it back.
func (l *Ledger) update(f func(b *book, now time.Time)) error {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(l.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(fh.Fd()), syscall.LOCK_UN) //nolint:errcheck
	raw, err := io.ReadAll(fh)
	if err != nil {
		return err
	}
	var b book
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			l.note("budget %s unreadable, starting it empty: %v", l.Path, err)
			b = book{}
		}
	}
	now := l.now()
	kept := b.Calls[:0]
	for _, e := range b.Calls {
		if now.Sub(e.At) < l.Window {
			kept = append(kept, e)
		}
	}
	b.Calls = kept
	sort.SliceStable(b.Calls, func(i, j int) bool { return b.Calls[i].At.Before(b.Calls[j].At) })
	f(&b, now)
	out, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := fh.Truncate(0); err != nil {
		return err
	}
	if _, err := fh.WriteAt(out, 0); err != nil {
		return err
	}
	return nil
}
