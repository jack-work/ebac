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

// cmdAddReleases mints a release watch (W7 item 3): one event per new release
// tag of a github.com repo, with its semver bump; every release wakes.
func cmdAddReleases(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-releases")
	name := fs.String("name", "", "critic name, e.g. releases-figaro")
	repo := fs.String("repo", "", "github.com owner/repo, e.g. jack-work/figaro")
	alert := fs.String("alert", "", "role to wake (@form); its target-aria is resolved at every send")
	aria := fs.String("aria", "", "seat cast into the critic's own role form (optional)")
	every := fs.Duration("every", 15*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if !strings.Contains(*repo, "/") {
		return fmt.Errorf("--repo %q: want owner/repo", *repo)
	}
	if *alert != "" && !strings.HasPrefix(*alert, "@") {
		return fmt.Errorf("--alert %q: want a role form (@id)", *alert)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Source: model.SourceReleases, Host: "github.com", CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), Releases: &model.ReleasesRef{Repo: *repo, AlertRole: *alert}, Stop: model.StopConditions{}}
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
	if *aria != "" {
		if err := model.ValidAriaID(*aria); err != nil {
			return fmt.Errorf("critic created (role %s), but --aria is bad: %w", formID, err)
		}
		if err := st.Fig.Cast(ctx, *aria, formID); err != nil {
			return fmt.Errorf("critic created (role %s), but casting %s failed: %w", formID, *aria, err)
		}
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("release watch %q: role %s, %s, alert %s, every %s\n", *name, formID, *repo, w.AlertTarget(), *every)
	return nil
}
