package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/tomb"
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
	p := &poll.Poller{Store: st, GH: forge.NewGH(g.gh, g.host), Tomb: &tomb.Tomb{Bin: env("EBAC_TOMB", "tomb"), Timeout: tomb.TeamsTombWait, Cred: forge.NewCred(env("EBAC_BAND_CRED", ""), forge.CredTeams)}, Fig: st.Fig, Out: os.Stdout, DryRun: *dry}
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
