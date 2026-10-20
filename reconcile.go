package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The reconciler.
//
// A critic exists in three places that can drift apart: a file in critics/, a
// role form in figaro, and a systemd timer. Nothing keeps them in step —
// selfcheck DETECTS divergence and stops there, deliberately, because it is
// meant to be safe to run from a develop-mode seat. This is the half that
// acts, and it runs rarely (every 30m) because every fix it makes is a write
// somebody else might be making at the same time.
//
// It also owns the roster form. That is the single-writer answer to the index
// problem: pollers never touch the roster, so there is no lost update, and
// `ebac ls` can read one form instead of interrogating every critic.
//
// It never deletes a form. Every form ebac mints carries a lease (lease.go)
// that its user renews, so an unclaimed form expires on its own.

type Finding struct {
	Rule    string `json:"rule"`
	Subject string `json:"subject"`
	Problem string `json:"problem"`
	Action  string `json:"action"`
	Fixed   bool   `json:"fixed"`
}

type ReconcileReport struct {
	Ran      string    `json:"ran"`
	Critics  int       `json:"critics"`
	Findings []Finding `json:"findings"`
	Fixed    int       `json:"fixed"`
	DryRun   bool      `json:"dry_run"`
}

func Reconcile(ctx context.Context, st *Store, fig *Figaro, out io.Writer, dryRun bool) (*ReconcileReport, error) {
	rep := &ReconcileReport{Ran: time.Now().UTC().Format(time.RFC3339), DryRun: dryRun}

	critics, err := st.ListCritics(ctx)
	if err != nil {
		return nil, err
	}
	rep.Critics = len(critics)

	add := func(rule, subject, problem, action string, fixed bool) {
		rep.Findings = append(rep.Findings, Finding{rule, subject, problem, action, fixed})
		if fixed {
			rep.Fixed++
		}
	}

	for _, c := range critics {
		if c.Runtime.Archived {
			continue // its form expires with its lease
		}

		// R1: the critic's role form is gone. Re-mint rather than leave a
		// critic that polls forever and delivers to nothing. Only the daemon
		// saying "not found" counts: on 2026-10-19 a transport fault made
		// every form unreadable and this rule minted a fresh, empty role
		// every half hour.
		if _, err := fig.Form(ctx, c.FormID); err != nil {
			if !errors.Is(err, ErrFormGone) {
				add("R1-form-unreadable", c.Name, "role form "+c.FormID+" unreadable: "+err.Error(), "left alone", false)
				continue
			}
			if dryRun {
				add("R1-form-missing", c.Name, "role form "+c.FormID+" unreadable", "would re-mint and re-project", false)
			} else {
				newID, mErr := fig.FormNew(ctx, "ebac-"+c.Name)
				if mErr != nil {
					add("R1-form-missing", c.Name, "role form "+c.FormID+" unreadable", "re-mint FAILED: "+mErr.Error(), false)
					continue
				}
				old := c.FormID
				c.FormID = newID
				if sErr := st.SaveCritic(ctx, c); sErr != nil {
					add("R1-form-missing", c.Name, "role form "+old+" unreadable", "re-minted "+newID+" but save FAILED: "+sErr.Error(), false)
					continue
				}
				add("R1-form-missing", c.Name, "role form "+old+" was gone", "re-minted as "+newID+" (SEAT IS EMPTY: ebac cast --critic "+c.Name+" --aria <id>)", true)
			}
			continue
		}

		// R5: the seat is gone. Never guess a replacement — say so loudly and
		// record it on the form so the next reader sees it too.
		form, _ := fig.Form(ctx, c.FormID)
		seat, _ := form["target-aria"].(string)
		switch {
		case seat == "":
			add("R5-seat", c.Name, "role has no target-aria", "cast one: ebac cast --critic "+c.Name+" --aria <id>", false)
		case !fig.AriaExists(ctx, seat):
			if !dryRun {
				_ = fig.SetJSON(ctx, c.FormID, "seat_lost", fmt.Sprintf("aria %s no longer resolves as of %s", seat, rep.Ran))
			}
			add("R5-seat", c.Name, "target-aria "+seat+" does not resolve", "re-cast: ebac cast --critic "+c.Name+" --aria <id>", false)
		default:
			if _, had := form["seat_lost"]; had && !dryRun {
				// The condition cleared. Remove the marker, or it becomes a
				// permanent accusation against a working seat.
				if err := fig.Unset(ctx, c.FormID, "seat_lost"); err == nil {
					add("R5-seat", c.Name, "stale seat_lost marker", "cleared", true)
				}
			}
		}

		// R3: a live critic with no armed timer never runs again, and nothing
		// else in the system notices.
		if !c.Runtime.Stopped {
			if ts := TimerState(c.Name); !ts.Armed() {
				if dryRun {
					add("R3-timer", c.Name, "timer "+ts.Display, "would re-arm", false)
				} else if err := EnableTimer(c.Name, 5*time.Minute); err != nil {
					add("R3-timer", c.Name, "timer "+ts.Display, "re-arm FAILED: "+err.Error(), false)
				} else {
					add("R3-timer", c.Name, "timer was "+ts.Display, "re-armed at 5m", true)
				}
			}
		}
	}

	// R6: rebuild the index. Unconditional — this is the only writer, so the
	// roster is exactly as fresh as the last reconcile, and it stamps its own
	// age so a stale one cannot pass for a current one.
	if !dryRun {
		if _, _, err := SyncRoster(ctx, st, fig, ""); err != nil {
			add("R6-roster", "roster", "rebuild failed", err.Error(), false)
		} else {
			add("R6-roster", "roster", "index rebuilt", rep.Ran, true)
		}
	}

	printReconcile(out, rep)
	return rep, nil
}

func printReconcile(out io.Writer, rep *ReconcileReport) {
	mode := ""
	if rep.DryRun {
		mode = " (DRY RUN)"
	}
	fmt.Fprintf(out, "ebac reconcile%s — %d critic(s), %d finding(s), %d fixed\n",
		mode, rep.Critics, len(rep.Findings), rep.Fixed)
	for _, f := range rep.Findings {
		mark := " "
		if f.Fixed {
			mark = "✓"
		}
		fmt.Fprintf(out, "  %s %-18s %-14s %s → %s\n", mark, f.Rule, f.Subject, f.Problem, f.Action)
	}
	if len(rep.Findings) == 0 {
		fmt.Fprintln(out, "  nothing to rectify")
	}
}

func cmdReconcile(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("reconcile")
	dry := fs.Bool("dry-run", false, "report what would change, change nothing")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := NewStore(NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	sink := io.Writer(os.Stdout)
	if *asJSON {
		sink = io.Discard
	}
	rep, err := Reconcile(ctx, st, st.Fig, sink, *dry)
	if err != nil {
		return err
	}
	if *asJSON {
		dumpJSON(rep)
	}
	return nil
}
