package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/bandstate"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddBandHub mints a band-hub mirror (plugin ebac-bandhub): a role form its
// studiers study, a timer, no seat (warden order 2026-11-09).
func cmdAddBandHub(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-band-hub")
	name := fs.String("name", "band-state", "critic name; the role form is ebac-<name>")
	socket := fs.String("socket", bandstate.DefaultSocket, "the hub's unix socket (this account must be a hub meta reader)")
	utility := fs.String("utility", "leob,ross", "the hub's utility nodes; every other node is heavy")
	repo := fs.String("repo", bandstate.DefaultRepo, "repo of the jobs' branch params, for the superseded check")
	every := fs.Duration("every", 45*time.Second, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	ref := &model.BandRef{Socket: *socket, Repo: *repo}
	for _, u := range strings.Split(*utility, ",") {
		if u = strings.TrimSpace(u); u != "" {
			ref.Utility = append(ref.Utility, u)
		}
	}
	w := &model.Critic{Name: *name, Source: model.SourceBandHub, Host: g.host, CreatedAt: time.Now().UTC(),
		FigaroSock: g.figaro, EverySec: int(every.Seconds()), Band: ref, Stop: model.StopConditions{}}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if err := st.SeedTrigger(ctx, w); err != nil {
		return fmt.Errorf("critic created (role %s), but %w", w.FormID, err)
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("band-hub mirror %q: role %s (no seat; studiers: figaro study %s), hub %s, every %s\n", *name, formID, formID, *socket, *every)
	return nil
}
