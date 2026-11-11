package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/relevance"
	"github.com/gluck/ebac/internal/source/scout"
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddScout mints the GitHub scout (source github-scout): no seat; it
// writes pr:<n> and issue:<n> entries on its own role form and the ledger
// outbox on the inputs form, and reads the board's owner marks.
func cmdAddScout(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-scout")
	name := fs.String("name", "effort-github", "critic name; the role form is ebac-<name>")
	authors := fs.String("authors", "", "GitHub logins whose PRs and issues are read, comma-separated (e.g. John-Kelliher,Chiqun-Zhang)")
	repo := fs.String("repo", "", "owner/repo on the forge host (e.g. bic/aether)")
	board := fs.String("board", "", "the board form (@id), read-only: owner marks for the merge checklist")
	inputs := fs.String("inputs", "", "the inputs form (@id): rule, ledger_ack read; ledger_outbox written")
	labels := fs.String("labels", "", "relevance: labels, comma-separated (e.g. effort-router)")
	title := fs.String("title-match", "", "relevance: title regex")
	paths := fs.String("paths", "", "relevance: changed-file globs, comma-separated (** spans directories)")
	var owners ownerFlags
	fs.Var(&owners, "owner", "AUTHOR[/TITLE_REGEX]=@ROLE, repeatable, first match wins: who gets a material change on an issue\n"+
		"its PRs link (e.g. 'John-Kelliher/(?i)ledger=@<ledger lead>' before 'John-Kelliher=@<knight>')")
	warn := fs.String("warn", "", "the role (@form) pinged once when open entries alone pass the form cap (the Decisions Minister)")
	host := fs.String("host", "microsoft.ghe.com", "forge host")
	every := fs.Duration("every", 2*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	sw := &model.ScoutWatch{Repo: *repo, Authors: splitList(*authors), Labels: splitList(*labels), TitleMatch: *title, Paths: splitList(*paths),
		Owners: owners, WarnTo: *warn}
	if len(sw.Authors) == 0 || !strings.Contains(sw.Repo, "/") {
		return fmt.Errorf("--authors and --repo owner/name are required")
	}
	if _, err := relevance.Compile(sw.TitleMatch, sw.Paths); err != nil {
		return err
	}
	if err := scout.CheckOwners(sw.Owners); err != nil {
		return err
	}
	if sw.WarnTo != "" && !strings.HasPrefix(sw.WarnTo, "@") {
		return fmt.Errorf("--warn %q: want a role form (@id)", sw.WarnTo)
	}
	for flag, v := range map[string]string{"--board": *board, "--inputs": *inputs} {
		if !strings.HasPrefix(v, "@") {
			return fmt.Errorf("%s %q: want a form (@id)", flag, v)
		}
	}
	if err := forge.CheckForgeHost(*host); err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Source: model.SourceScout, Host: *host, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), BoardForm: *board, InputsForm: *inputs, Scout: sw, Stop: model.StopConditions{}}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("scout %q: role %s, %s in %s, board %s (read-only), inputs %s, every %s\n", *name, formID,
		strings.Join(sw.Authors, ", "), sw.Repo, *board, *inputs, *every)
	return nil
}

// ownerFlags are repeated --owner AUTHOR[/TITLE_REGEX]=@ROLE flags.
type ownerFlags []model.OwnerRule

func (o *ownerFlags) String() string { return fmt.Sprint(*o) }

func (o *ownerFlags) Set(v string) error {
	i := strings.LastIndex(v, "=")
	if i < 0 {
		return fmt.Errorf("--owner %q: want AUTHOR[/TITLE_REGEX]=@ROLE", v)
	}
	author, title, _ := strings.Cut(v[:i], "/")
	*o = append(*o, model.OwnerRule{Author: author, TitleMatch: title, Role: v[i+1:]})
	return nil
}
