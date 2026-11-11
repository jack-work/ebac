package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddKeelBranches mints a keel-branches critic: one child form per work
// branch of a keel repo; no seat of its own.
func cmdAddKeelBranches(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-keel-branches")
	name := fs.String("name", "", "critic name, e.g. aether-work: branch forms are ebac-<name>-<owner>--<topic>")
	repo := fs.String("repo", "", "keel repo, owner/name (e.g. aether/aether)")
	base := fs.String("base", "main", "the branch work is measured against")
	prefix := fs.String("prefix", "work/", "the branches followed: <prefix><owner>/<topic>")
	reviewer := fs.String("reviewer", "", "the role (@form) pinged when code_ready turns true and on each new head after (the Reviewer)")
	every := fs.Duration("every", 2*time.Minute, "poll interval")
	keelURL := fs.String("keel-url", "", "read keel over HTTPS at this base (https://...) instead of the unix door")
	tokenCmd := fs.String("token-cmd", "", "with --keel-url: the command that prints the bearer token (run without a shell)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if owner, nm, ok := strings.Cut(*repo, "/"); !ok || owner == "" || nm == "" || strings.Contains(nm, "/") {
		return fmt.Errorf("--repo %q: want owner/name", *repo)
	}
	if *reviewer != "" && !strings.HasPrefix(*reviewer, "@") {
		return fmt.Errorf("--reviewer %q: want a role form (@id)", *reviewer)
	}
	ref := &model.KeelRef{Repo: *repo, URL: strings.TrimSpace(*keelURL), TokenCmd: strings.Fields(*tokenCmd)}
	if err := validTransport(ref); err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Source: model.SourceKeelBranches, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), Stop: model.StopConditions{},
		Branches: &model.KeelBranches{Repo: *repo, Base: *base, Prefix: *prefix, Reviewer: *reviewer, URL: ref.URL, TokenCmd: ref.TokenCmd}}
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
	fmt.Printf("keel branches %q: role %s, %s %s<owner>/<topic> against %s, reviewer %s, every %s\n", *name, formID, *repo, *prefix, *base, *reviewer, *every)
	return nil
}
