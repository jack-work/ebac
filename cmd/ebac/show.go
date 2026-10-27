package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// ---------- ls / show ----------

func cmdLs(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("ls")
	asJSON := fs.Bool("j", false, "JSON")
	live := fs.Bool("live", false, "rebuild from source instead of reading the published index")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	fig := st.Fig

	var (
		rows   []fleet.RosterRow
		sum    fleet.RosterSummary
		age    time.Duration
		source = "index"
	)
	if *live {
		rows, sum, err = fleet.BuildRoster(ctx, st, fig, "")
		source = "live"
	} else {
		rows, sum, age, err = fleet.ReadRoster(ctx, st, fig)
		if err != nil {
			// Falling back is right, but doing it SILENTLY is not: an
			// operator must never mistake a rebuild for the index.
			fmt.Fprintf(os.Stderr, "ebac: %v; rebuilding live\n", err)
			rows, sum, err = fleet.BuildRoster(ctx, st, fig, "")
			source = "live (index unavailable)"
		}
	}
	if err != nil {
		return err
	}

	if *asJSON {
		fleet.DumpJSON(map[string]any{"source": source, "age_seconds": int(age.Seconds()), "summary": sum, "critics": rows})
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("no active critics (ebac add --name ...)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CRITIC\tMODE\tSEAT\tPRS\tOPEN\tUNRES\tROUND\tTIMER\tHEALTH")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			r.Critic, r.Mode, r.Seat, r.PRs, r.Open, r.Unresolved, r.Round, r.Timer, r.Health)
	}
	tw.Flush()
	fmt.Printf("\n%d critic(s), %d PRs (%d open), %d unresolved threads, %d awaiting a reply\n",
		sum.Critics, sum.PRs, sum.OpenPRs, sum.Unresolved, sum.Awaiting)

	switch {
	case source != "index":
		fmt.Printf("source: %s\n", source)
	case age > 45*time.Minute:
		// The reconciler runs every 30m. Past 45 the index is not merely
		// old, something has stopped.
		fmt.Printf("source: index, STALE by %s — is ebac-reconcile.timer running? (ebac ls --live)\n", age.Round(time.Minute))
	default:
		fmt.Printf("source: index, %s old\n", age.Round(time.Second))
	}
	if sum.Unhealthy > 0 {
		fmt.Printf("%d unhealthy — ebac selfcheck\n", sum.Unhealthy)
	}
	return nil
}

func cmdShow(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	snap, _ := st.LoadSnapshot(ctx, *name)
	if *asJSON {
		fleet.DumpJSON(map[string]any{"critic": w, "snapshot": snap})
		return nil
	}
	fig := st.Fig
	seat := "(empty)"
	if form, err := fig.Form(ctx, w.FormID); err == nil {
		if t, ok := form["target-aria"].(string); ok && t != "" {
			seat = t
		}
	}
	// AwaitingUs("") is not "unknown me", it is "every non-empty author
	// counts as not-me" (strings.EqualFold(x, "") is false for any
	// non-empty x) -- so an unresolved thread and an awaiting-a-reply
	// thread became the same column here even though poll.go resolves a
	// real identity before ever computing the same Counts. Same mistake
	// that mattered enough to fix in poll.go; this is the one display
	// path that never got it. Best-effort: a Whoami failure degrades to
	// the old (wrong-but-not-crashing) behavior rather than erroring the
	// whole command.
	fmt.Printf("critic     %s (%s)\n", w.Name, w.Mode)
	fmt.Printf("role      %s -> %s\n", w.FormID, seat)
	fmt.Printf("timer     %s, next %s\n", systemd.TimerState(w.Name).Display, systemd.NextElapse(w.Name))
	fmt.Printf("round     %d, last success %s\n", w.Runtime.Round, stamp(w.Runtime.LastSuccess))
	fmt.Printf("stop when %s\n", brief.DescribeStop(w.Stop))
	if w.Runtime.LastError != "" {
		fmt.Printf("error     %s\n", w.Runtime.LastError)
	}
	if n := len(w.Runtime.PendingEvents); n > 0 {
		fmt.Printf("pending   %d undelivered event(s), retry %d, next %s\n",
			n, w.Runtime.RetryCount, stamp(w.Runtime.NextRetryAt))
	}
	if a := w.Author; a != nil {
		fmt.Printf("author    %s in %s since %s, alert %s\n", a.Login, strings.Join(a.Repos, ","), a.Since.UTC().Format(time.RFC3339), w.AlertTarget())
		if s := w.Runtime.Author; s != nil {
			fmt.Printf("seen      %d PR(s)\n", len(s.Seen))
		}
		return nil
	}
	if w.Chat != nil {
		fmt.Printf("chat      %s (%s), quiet: %s\n", w.Chat.Topic, w.Chat.ID, strings.Join(w.Chat.Quiet, ", "))
		if t := w.Runtime.Teams; t != nil {
			fmt.Printf("seen      %d message(s), gone streak %d\n", len(t.Seen), t.GoneStreak)
		}
		return nil
	}
	me, _ := forge.NewGH(g.gh, w.Host).Whoami(ctx)
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PR\tSTATE\tHEAD\tTHREADS\tUNRES\tAWAITING\tTITLE")
	for _, row := range prTableRows(w.PRs, snap, me) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			row.Key, row.State, row.Head, row.Threads, row.Unresolved, row.Awaiting, model.FirstLine(row.Title))
	}
	tw.Flush()
	return nil
}

// prTableRow is one line of `ebac show`'s PR table. Split out from cmdShow
// so the AWAITING-vs-UNRES computation is testable without a live GH call.
type prTableRow struct {
	Key                           string
	State, Head, Title            string
	Threads, Unresolved, Awaiting int
}

func prTableRows(refs []model.PRRef, snap *model.Snapshot, me string) []prTableRow {
	rows := make([]prTableRow, 0, len(refs))
	for _, r := range refs {
		row := prTableRow{Key: string(r.Key()), State: "?", Head: "-", Title: r.Title}
		if snap != nil {
			if p, ok := snap.PRs[r.Key()]; ok {
				c := p.Counts(me)
				row.State, row.Head, row.Title = p.State, model.Short(p.HeadSHA), p.Title
				row.Threads, row.Unresolved, row.Awaiting = c.Threads, c.Unresolved, c.AwaitingUs
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
