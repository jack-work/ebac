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
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddBoard mints a board-sync critic (source github-prs): no seat; it
// writes child PR forms, the board form and the inputs form's ledger outbox.
func cmdAddBoard(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-board")
	name := fs.String("name", "board-sync", "critic name; the role form is ebac-<name>")
	author := fs.String("author", "", "GitHub login whose PRs get forms (e.g. John-Kelliher)")
	repo := fs.String("repo", "", "owner/repo on the forge host (e.g. bic/aether)")
	board := fs.String("board", "", "the board form it reconciles (@id)")
	inputs := fs.String("inputs", "", "the inputs form (@id): rule, needs, ledger_outbox, ledger_ack")
	judgeTo := fs.String("judge", "", "the role (@id) that gets each new judgement line (the Decisions Minister)")
	host := fs.String("host", "microsoft.ghe.com", "forge host")
	every := fs.Duration("every", 2*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if *author == "" || !strings.Contains(*repo, "/") {
		return fmt.Errorf("--author and --repo owner/name are required")
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
	w := &model.Critic{Name: *name, Source: model.SourceBoard, Host: *host, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), BoardForm: *board, InputsForm: *inputs,
		BoardWatch: &model.BoardWatch{Author: *author, Repo: *repo, JudgeTo: *judgeTo}, Stop: model.StopConditions{}}
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
	fmt.Printf("board sync %q: role %s, %s's PRs in %s, board %s, inputs %s, every %s\n", *name, formID, *author, *repo, *board, *inputs, *every)
	return nil
}
