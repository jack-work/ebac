package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// The roster is a special role: one form holding a view of every active PR
// review. Cast an overseer into it and that aria sees the whole fleet change
// under it without subscribing to each critic.
//
// It is rebuilt WHOLESALE from the state directory by one writer, never
// patched by each critic. Concurrent pollers appending their own row to a
// shared form is a lost-update bug waiting for a busy afternoon.

type RosterRow struct {
	Critic     string `json:"critic"`
	Form       string `json:"form"`
	Seat       string `json:"seat"`
	Mode       string `json:"mode"`
	PRs        int    `json:"prs"`
	Open       int    `json:"open"`
	Unresolved int    `json:"unresolved"`
	Awaiting   int    `json:"awaiting_reply"`
	Round      int    `json:"round"`
	LastPolled string `json:"last_polled,omitempty"`
	Timer      string `json:"timer"`
	Health     string `json:"health"`
	Pending    int    `json:"pending_delivery,omitempty"`
}

type RosterSummary struct {
	Critics     int    `json:"critics"`
	PRs         int    `json:"prs"`
	OpenPRs     int    `json:"open_prs"`
	Unresolved  int    `json:"unresolved_threads"`
	Awaiting    int    `json:"awaiting_reply"`
	Unhealthy   int    `json:"unhealthy"`
	SeatsEmpty  int    `json:"seats_empty"`
	GeneratedAt string `json:"generated_at"`
}

// RosterForm finds the roster by kind, minting it on first use. Like the
// queue, it is found by property, never through a pointer on disk.
func RosterForm(ctx context.Context, fig *figaro.Figaro) (string, error) {
	found, err := FindFormsByKind(ctx, fig, "ebac.roster")
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		id, err := fig.FormNew(ctx, "ebac-roster")
		if err != nil {
			return "", err
		}
		if err := fig.SetJSON(ctx, id, "kind", "ebac.roster"); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", fmt.Errorf("%d forms claim kind ebac.roster (%s): remove the stale ones", len(found), strings.Join(found, ", "))
}

func BuildRoster(ctx context.Context, st *form.Store, fig *figaro.Figaro, me string) ([]RosterRow, RosterSummary, error) {
	critics, err := st.ListCritics(ctx)
	if err != nil {
		return nil, RosterSummary{}, err
	}
	var rows []RosterRow
	var sum RosterSummary
	for _, w := range critics {
		if w.Runtime.Archived {
			continue
		}
		row := RosterRow{
			Critic: w.Name, Form: w.FormID, Mode: modeLabel(w),
			PRs: len(w.PRs), Round: w.Runtime.Round,
			Pending: len(w.Runtime.PendingEvents),
		}
		if !w.Runtime.LastPolled.IsZero() {
			row.LastPolled = w.Runtime.LastPolled.UTC().Format(time.RFC3339)
		}
		timer := systemd.TimerState(w.Name)
		row.Timer = timer.Display

		// Seat health is the SAME question the selfcheck asks, asked the
		// same way. Two instruments that disagree about one fact are worse
		// than one instrument: the roster used to score an unreachable seat
		// as "ok" because it only tested for emptiness.
		seatOK := false
		if form, err := fig.Form(ctx, w.FormID); err == nil {
			if t, ok := form["target-aria"].(string); ok && t != "" {
				row.Seat = t
				seatOK = fig.AriaExists(ctx, t)
			}
		}
		if row.Seat == "" {
			row.Seat = "(empty)"
			sum.SeatsEmpty++
		} else if !seatOK {
			row.Seat += " (unreachable)"
			sum.SeatsEmpty++
		}
		if snap, _ := st.LoadSnapshot(ctx, w.Name); snap != nil {
			for _, pr := range snap.PRs {
				if !pr.Closed() {
					row.Open++
				}
				c := pr.Counts(me)
				row.Unresolved += c.Unresolved
				row.Awaiting += c.AwaitingUs
			}
		}
		row.Health = health(w, row, timer)
		if row.Health != "ok" {
			sum.Unhealthy++
		}
		sum.PRs += row.PRs
		sum.OpenPRs += row.Open
		sum.Unresolved += row.Unresolved
		sum.Awaiting += row.Awaiting
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Critic < rows[j].Critic })
	sum.Critics = len(rows)
	sum.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	return rows, sum, nil
}

func health(w *model.Critic, row RosterRow, timer systemd.TimerStatus) string {
	switch {
	case w.Runtime.Stopped:
		return "stopped: " + w.Runtime.StopReason
	case row.Seat == "(empty)":
		return "no seat cast"
	case strings.HasSuffix(row.Seat, "(unreachable)"):
		return "seat aria is gone; re-cast"
	case !timer.Armed():
		return "timer not armed (" + timer.Display + ")"
	case w.Runtime.ConsecutiveFailures > 0:
		return fmt.Sprintf("%d consecutive failures", w.Runtime.ConsecutiveFailures)
	case len(w.Runtime.PendingEvents) > 0 && w.Runtime.RetryCount > 0:
		return fmt.Sprintf("delivery retrying (%d)", w.Runtime.RetryCount)
	case w.Runtime.LastError != "":
		return "error: " + model.FirstLine(w.Runtime.LastError)
	}
	return "ok"
}

func SyncRoster(ctx context.Context, st *form.Store, fig *figaro.Figaro, me string) (string, RosterSummary, error) {
	id, err := RosterForm(ctx, fig)
	if err != nil {
		return "", RosterSummary{}, err
	}
	figaro.Renew(ctx, fig, id)
	rows, sum, err := BuildRoster(ctx, st, fig, me)
	if err != nil {
		return "", RosterSummary{}, err
	}
	// Top-level keys, whole values. Same rule as everywhere else.
	if err := fig.SetJSON(ctx, id, "kind", "ebac.roster"); err != nil {
		return "", sum, err
	}
	if err := fig.SetJSON(ctx, id, "summary", sum); err != nil {
		return "", sum, err
	}
	if err := fig.SetJSON(ctx, id, "critics", rows); err != nil {
		return "", sum, err
	}
	if err := fig.SetJSON(ctx, id, "harness", map[string]string{
		"refresh":   "ebac roster --sync",
		"detail":    "ebac show --critic <name>",
		"read_only": "This is a VIEW. Do not act on the pull requests it names.",
	}); err != nil {
		return "", sum, err
	}
	return id, sum, nil
}

func dumpJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// ReadRoster returns the published index without interrogating a single
// critic: one form read instead of N figaro round-trips plus N snapshot
// loads. The reconciler is its only writer.
//
// It returns the index's OWN age. A stale index and a fresh one are otherwise
// identical on screen, and an operator reading two-day-old health as current
// is worse served than one told the index is cold — which is exactly how the
// roster misreported 0 PRs on a 4-PR fleet for two days.
func ReadRoster(ctx context.Context, st *form.Store, fig *figaro.Figaro) (rows []RosterRow, sum RosterSummary, age time.Duration, err error) {
	found, err := FindFormsByKind(ctx, fig, "ebac.roster")
	if err != nil {
		return nil, sum, 0, err
	}
	if len(found) != 1 {
		return nil, sum, 0, fmt.Errorf("%d roster forms: run `ebac reconcile`", len(found))
	}
	id := found[0]
	form, err := fig.Form(ctx, id)
	if err != nil {
		return nil, sum, 0, fmt.Errorf("roster form %s unreadable: %w", id, err)
	}
	if _, ok := form["critics"]; !ok {
		return nil, sum, 0, fmt.Errorf("roster form %s not yet published: run `ebac reconcile`", id)
	}
	raw, err := json.Marshal(form["critics"])
	if err != nil {
		return nil, sum, 0, err
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		// A truncated roster is a cut object, so this is where elision
		// surfaces. Say which failure it was rather than reporting an
		// empty fleet.
		return nil, sum, 0, fmt.Errorf("roster index unparseable (elided at %d chars?): %w", brief.FormValueElisionLimit, err)
	}
	rawSum, err := json.Marshal(form["summary"])
	if err == nil {
		_ = json.Unmarshal(rawSum, &sum)
	}
	if sum.GeneratedAt != "" {
		if t, e := time.Parse(time.RFC3339, sum.GeneratedAt); e == nil {
			age = time.Since(t)
		}
	}
	return rows, sum, age, nil
}

// modeLabel renders the mode with a "+w" suffix when the critic may act on its
// own PRs, so `ebac ls` shows the grant at a glance rather than hiding it one
// `ebac show` away. A capability nobody can see is a capability nobody audits.
func modeLabel(w *model.Critic) string {
	s := string(w.Mode)
	if w.Write {
		s += "+w"
	}
	if w.Approve {
		s += "+a"
	}
	return s
}
