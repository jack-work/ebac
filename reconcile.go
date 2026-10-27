package main

import (
	"context"
	"io"
	"os"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/form"
)

func cmdReconcile(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("reconcile")
	dry := fs.Bool("dry-run", false, "report what would change, change nothing")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	sink := io.Writer(os.Stdout)
	if *asJSON {
		sink = io.Discard
	}
	rep, err := fleet.Reconcile(ctx, st, st.Fig, sink, *dry)
	if err != nil {
		return err
	}
	if *asJSON {
		fleet.DumpJSON(rep)
	}
	return nil
}
