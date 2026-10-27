package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// ---------- CLI ----------

func cmdQueue(ctx context.Context, g globals, args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	fig := st.Fig
	defer fig.Close()
	formID, err := fleet.QueueForm(ctx, fig)
	if err != nil {
		return err
	}

	switch sub {
	case "add":
		fs := newFlagSet("queue add")
		var prs stringList
		fs.Var(&prs, "pr", "PR url or owner/repo#number (repeatable)")
		prio := fs.Int("priority", 0, "higher goes first; ties break FIFO")
		mode := fs.String("mode", "", "override the queue's default mode")
		note := fs.String("note", "", "why this is queued")
		_ = fs.Parse(args)
		if len(prs) == 0 {
			return fmt.Errorf("give at least one --pr")
		}
		for _, raw := range prs {
			ref, err := model.ParsePRRef(raw)
			if err != nil {
				return err
			}
			it, err := fleet.Enqueue(ctx, st, fig, formID, ref.HostOf(g.host), ref, *prio, *mode, *note)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
				continue
			}
			fmt.Printf("queued %s (priority %d, seq %d)\n", it.PRKey(), it.Priority, it.Seq)
		}
		return nil

	case "rm":
		if len(args) == 0 {
			return fmt.Errorf("give an item id (ebac queue ls)")
		}
		if err := fleet.RemoveItem(ctx, fig, formID, args[0]); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[0])
		return nil

	case "config":
		fs := newFlagSet("queue config")
		max := fs.Int("max-active", -1, "ceiling on concurrent critics")
		mode := fs.String("mode", "", "default mode for promoted critics")
		outfit := fs.String("outfit", "", "default outfit for minted seats")
		issue := fs.String("issue-repo", "", "default issue repo")
		interval := fs.String("interval", "", "heartbeat for promoted critics")
		_ = fs.Parse(args)
		_, cfg, _, err := fleet.ReadQueue(ctx, fig, formID)
		if err != nil {
			return err
		}
		if *max >= 0 {
			cfg.MaxActive = *max
		}
		if *mode != "" {
			cfg.Mode = *mode
		}
		if *outfit != "" {
			cfg.Outfit = *outfit
		}
		if *issue != "" {
			cfg.IssueRepo = *issue
		}
		if *interval != "" {
			if _, err := time.ParseDuration(*interval); err != nil {
				return fmt.Errorf("--interval: %w", err)
			}
			cfg.Interval = *interval
		}
		if err := fig.SetJSON(ctx, formID, "config", cfg); err != nil {
			return err
		}
		fleet.DumpJSON(cfg)
		return nil

	case "promote":
		fs := newFlagSet("queue promote")
		dry := fs.Bool("dry-run", false, "decide, change nothing")
		_ = fs.Parse(args)
		p := &fleet.Promoter{Store: st, Fig: fig, GH: forge.NewGH(g.gh, g.host), Out: os.Stdout,
			Host: g.host, Dry: *dry}
		_, err := p.Promote(ctx)
		return err

	case "ls":
		fs := newFlagSet("queue ls")
		asJSON := fs.Bool("j", false, "JSON")
		_ = fs.Parse(args)
		items, cfg, version, err := fleet.ReadQueue(ctx, fig, formID)
		if err != nil {
			return err
		}
		critics, _ := st.ListCritics(ctx)
		active := 0
		for _, c := range critics {
			if !c.Runtime.Archived {
				active++
			}
		}
		if *asJSON {
			fleet.DumpJSON(map[string]any{"form": formID, "version": version,
				"config": cfg, "active": active, "items": items})
			return nil
		}
		fmt.Printf("queue %s (v%d) — %d/%d active critics\n\n", formID, version, active, cfg.MaxActive)
		if len(items) == 0 {
			fmt.Println("empty (ebac queue add --pr <url>)")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPR\tPRIO\tSEQ\tSTATE\tCRITIC\tNOTE")
		for _, it := range items {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
				it.ID, it.PRKey(), it.Priority, it.Seq, it.State, it.Critic, model.FirstLine(it.Note))
		}
		tw.Flush()
		return nil
	}
	return fmt.Errorf("unknown: ebac queue %s (add|ls|rm|config|promote)", sub)
}
