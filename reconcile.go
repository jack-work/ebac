package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
// Detection is by PROPERTY, never by name: an ebac-owned form is one carrying
// `kind` = ebac.critic or ebac.roster. A form named "ebac-something" that we
// did not write is not ours, and a form of ours that somebody renamed still is.

type Finding struct {
	Rule    string `json:"rule"`
	Subject string `json:"subject"`
	Problem string `json:"problem"`
	Action  string `json:"action"`
	Fixed   bool   `json:"fixed"`
}

type ReconcileReport struct {
	Ran        string    `json:"ran"`
	Critics    int       `json:"critics"`
	OwnedForms int       `json:"owned_forms"`
	Findings   []Finding `json:"findings"`
	Fixed      int       `json:"fixed"`
	DryRun     bool      `json:"dry_run"`
}

func Reconcile(ctx context.Context, st *Store, fig *Figaro, out io.Writer, dryRun, prune bool) (*ReconcileReport, error) {
	rep := &ReconcileReport{Ran: time.Now().UTC().Format(time.RFC3339), DryRun: dryRun}

	critics, err := st.ListCritics()
	if err != nil {
		return nil, err
	}
	rep.Critics = len(critics)

	// Index every form we own, by id, with the kind it declares.
	ownedKind := map[string]string{}
	ids, err := fig.ListFormIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate forms: %w", err)
	}
	for _, id := range ids {
		f, err := fig.Form(ctx, id)
		if err != nil {
			continue
		}
		if k, _ := f["kind"].(string); strings.HasPrefix(k, "ebac.") {
			ownedKind[id] = k
		}
	}
	rep.OwnedForms = len(ownedKind)

	add := func(rule, subject, problem, action string, fixed bool) {
		rep.Findings = append(rep.Findings, Finding{rule, subject, problem, action, fixed})
		if fixed {
			rep.Fixed++
		}
	}

	claimed := map[string]bool{}
	rosterID, _ := st.rosterFormID()
	if rosterID != "" {
		claimed[rosterID] = true
	}

	for _, c := range critics {
		claimed[c.FormID] = true

		// R1: the critic's role form is gone. Re-mint rather than leave a
		// critic that polls forever and delivers to nothing.
		if _, err := fig.Form(ctx, c.FormID); err != nil {
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
				if sErr := st.SaveCritic(c); sErr != nil {
					add("R1-form-missing", c.Name, "role form "+old+" unreadable", "re-minted "+newID+" but save FAILED: "+sErr.Error(), false)
					continue
				}
				snap, _ := st.LoadSnapshot(c.Name)
				p := &Poller{Store: st, Fig: fig, Out: io.Discard}
				p.projectAll(ctx, c, snap, &Delta{})
				add("R1-form-missing", c.Name, "role form "+old+" was gone", "re-minted as "+newID+" (SEAT IS EMPTY: ebac cast --critic "+c.Name+" --aria <id>)", true)
				claimed[newID] = true
			}
			continue
		}

		if c.Runtime.Archived {
			// R4: an archived critic has no business holding a form.
			if prune && !dryRun {
				if err := fig.FormRemove(ctx, c.FormID); err == nil {
					add("R4-archived-form", c.Name, "archived critic still holds "+c.FormID, "removed", true)
				}
			} else {
				add("R4-archived-form", c.Name, "archived critic still holds "+c.FormID, "run with --prune to remove", false)
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

	// R2: a form of ours that no critic claims. This is the only rule that can
	// destroy something, so it needs --prune AND it refuses to touch the
	// roster.
	for id, kind := range ownedKind {
		if claimed[id] {
			continue
		}
		if kind == "ebac.roster" {
			add("R2-orphan-form", id, "a second roster form exists", "left alone; check state/roster.json", false)
			continue
		}
		if prune && !dryRun {
			if err := fig.FormRemove(ctx, id); err == nil {
				add("R2-orphan-form", id, "no critic claims this "+kind, "removed", true)
				continue
			}
		}
		add("R2-orphan-form", id, "no critic claims this "+kind, "run with --prune to remove", false)
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
	fmt.Fprintf(out, "ebac reconcile%s — %d critic(s), %d owned form(s), %d finding(s), %d fixed\n",
		mode, rep.Critics, rep.OwnedForms, len(rep.Findings), rep.Fixed)
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

// rosterFormID reads the id without minting one, so reconcile can tell
// "no roster yet" from "roster is unreadable".
func (s *Store) rosterFormID() (string, error) {
	var m rosterMeta
	if err := readJSON(s.rosterPath(), &m); err != nil {
		return "", err
	}
	return m.FormID, nil
}

func cmdReconcile(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("reconcile")
	dry := fs.Bool("dry-run", false, "report what would change, change nothing")
	prune := fs.Bool("prune", false, "allow removal of orphaned and archived forms")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	sink := io.Writer(os.Stdout)
	if *asJSON {
		sink = io.Discard
	}
	rep, err := Reconcile(ctx, st, NewFigaro(g.figaro), sink, *dry, *prune)
	if err != nil {
		return err
	}
	if *asJSON {
		dumpJSON(rep)
	}
	return nil
}
