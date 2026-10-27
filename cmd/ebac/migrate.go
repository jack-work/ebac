package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/forge"
	ebacform "github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// cmdMigrate moves critics from the pre-form layout (critics/<name>.json and
// snapshots/<name>.json under the state root) into their role forms, one
// patch each, and moves the files to archive/migrated-<stamp>/.
func cmdMigrate(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "report only")
	_ = fs.Parse(args)
	st, err := ebacform.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	dir := filepath.Join(st.Root, "critics")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		fmt.Println("nothing to migrate")
		return nil
	}
	if err != nil {
		return err
	}
	dest := filepath.Join(st.Root, "archive", "migrated-"+time.Now().UTC().Format("20060102T150405Z"))
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		var w model.Critic
		if err := ebacform.ReadJSON(filepath.Join(dir, e.Name()), &w); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		var snap *model.Snapshot
		snapPath := filepath.Join(st.Root, "snapshots", name+".json")
		if _, err := os.Stat(snapPath); err == nil {
			snap = &model.Snapshot{}
			if err := ebacform.ReadJSON(snapPath, snap); err != nil {
				return fmt.Errorf("%s: %w", snapPath, err)
			}
		}
		if _, err := st.ReadForm(ctx, w.FormID); err != nil {
			return fmt.Errorf("%s: role form %s: %w", name, w.FormID, err)
		}
		prs := 0
		if snap != nil {
			prs = len(snap.PRs)
		}
		if *dry {
			fmt.Printf("would migrate %s into %s (%d PRs)\n", name, w.FormID, prs)
			continue
		}
		if st.Me == "" {
			if me, err := forge.NewGH(g.gh, w.Host).Whoami(ctx); err == nil {
				st.Me = me
			}
		}
		if err := st.Save(ctx, &w, snap, nil); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
		if snap != nil {
			if err := os.Rename(snapPath, filepath.Join(dest, name+".snapshot.json")); err != nil {
				return err
			}
		}
		n++
		fmt.Printf("migrated %s into %s (%d PRs)\n", name, w.FormID, prs)
	}
	if !*dry {
		for _, leftover := range []string{"critics", "snapshots"} {
			_ = os.Remove(filepath.Join(st.Root, leftover))
		}
		_ = os.Remove(filepath.Join(st.Root, "roster.json"))
		fmt.Printf("%d critic(s) migrated; files kept in %s\n", n, dest)
	}
	return nil
}

// cmdCompact rewrites a critic's form without comment bodies. It first
// deletes prs with a patch that carries no old value, because a normal
// delete record holds the deleted value and an oversized form cannot take
// one; then it writes the compact prs from what it read.
func cmdCompact(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	dry := fs.Bool("dry-run", false, "show the delete patch and stop")
	_ = fs.Parse(args)
	st, err := ebacform.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	f, err := st.FindCritic(ctx, *name)
	if err != nil {
		return err
	}
	if w, err := f.Critic(); err == nil {
		if me, err := forge.NewGH(g.gh, w.Host).Whoami(ctx); err == nil {
			st.Me = me
		}
	}
	return fleet.CompactForm(ctx, st, f, *dry)
}
