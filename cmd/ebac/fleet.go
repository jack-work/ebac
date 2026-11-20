package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/forge"
)

// ---------- roster ----------

func cmdRoster(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("roster", flag.ExitOnError)
	sync := fs.Bool("sync", false, "rebuild the roster form from state")
	cast := fs.String("cast", "", "cast this aria into the roster role")
	mint := fs.Bool("mint", false, "mint an overseer aria and cast it")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	fig := st.Fig
	if *sync || *cast != "" || *mint {
		id, sum, err := fleet.SyncRoster(ctx, st, fig, "")
		if err != nil {
			return err
		}
		fmt.Printf("roster %s: %d critic(es), %d PRs (%d open), %d unhealthy\n",
			id, sum.Critics, sum.PRs, sum.OpenPRs, sum.Unhealthy)
		seat := *cast
		if seat == "" && *mint {
			seat, err = fig.NewAria(ctx, "")
			if err != nil {
				return err
			}
			fmt.Printf("minted overseer %s\n", seat)
		}
		if seat != "" {
			if err := fig.Cast(ctx, seat, id); err != nil {
				return err
			}
			fmt.Printf("cast %s into the roster role %s\n", seat, id)
		}
		return nil
	}
	rows, sum, err := fleet.BuildRoster(ctx, st, fig, "")
	if err != nil {
		return err
	}
	if *asJSON {
		fleet.DumpJSON(map[string]any{"summary": sum, "critics": rows})
		return nil
	}
	fleet.DumpJSON(rows)
	return nil
}

// ---------- issue / gc ----------

func cmdIssue(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	title := fs.String("title", "", "issue title")
	bodyFile := fs.String("body-file", "", "the issue body: - for stdin (the only form through the sudo door), or a path")
	repo := fs.String("repo", "", "override the critic's issue repo")
	var labels stringList
	fs.Var(&labels, "label", "label (repeatable)")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	target := *repo
	if target == "" {
		target = w.IssueRepo
	}
	if target == "" {
		return fmt.Errorf("no issue repo: pass --repo owner/name, or set one on the critic")
	}
	if *title == "" || *bodyFile == "" {
		return fmt.Errorf("--title and --body-file are both required")
	}
	var body []byte
	if *bodyFile == "-" {
		body, err = io.ReadAll(os.Stdin) // the only form the sudo door lets through
	} else {
		body, err = os.ReadFile(*bodyFile)
	}
	if err != nil {
		return err
	}
	// Stamp provenance so a human reading the issue knows what produced it
	// and can reproduce the round.
	full := fmt.Sprintf("%s\n\n---\nFiled by ebac (critic `%s`, round %d, role %s) on %s.\n",
		strings.TrimRight(string(body), "\n"), w.Name, w.Runtime.Round, w.FormID,
		time.Now().UTC().Format(time.RFC3339))
	url, err := forge.NewGH(g.gh, g.host).CreateIssue(ctx, target, *title, full, labels)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}
