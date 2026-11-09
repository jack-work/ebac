package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/poll"
)

// ---------- poll ----------

func cmdPoll(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	dry := fs.Bool("dry-run", false, "reconcile and persist state, but never invoke figaro")
	all := fs.Bool("all", false, "poll every live critic in sequence")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	pl, err := plugin.NewRunner()
	if err != nil {
		return err
	}
	p := &poll.Poller{Store: st, Fig: st.Fig, Plugins: pl, Out: os.Stdout, DryRun: *dry, GH: g.gh, Host: g.host}
	if *all {
		ws, err := st.ListCritics(ctx)
		if err != nil {
			return err
		}
		var firstErr error
		for _, w := range ws {
			if w.Runtime.Archived {
				continue
			}
			if err := p.Poll(ctx, w.Name); err != nil {
				fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}
	if *name == "" {
		return fmt.Errorf("--critic is required (or --all)")
	}
	return p.Poll(ctx, *name)
}
