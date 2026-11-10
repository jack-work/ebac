package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/source/author"
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddAuthor mints an author watch: role form, critic, optional seat, timer.
func cmdAddAuthor(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-author")
	name := fs.String("name", "", "critic name, e.g. author-chiqun")
	login := fs.String("login", "", "GitHub login whose PRs to watch")
	since := fs.String("since", "", "window start, RFC 3339 (e.g. 2026-10-15T00:00:00Z)")
	until := fs.String("until", "", "window end, RFC 3339 (optional; the watch stops after it)")
	repos := fs.String("repo", "", "comma-separated owner/repo list")
	alert := fs.String("alert", "", "role to wake (@form); its target-aria is resolved at every send")
	aria := fs.String("aria", "", "seat cast into the critic's own role form (optional)")
	host := fs.String("host", "microsoft.ghe.com", "forge host")
	every := fs.Duration("every", 5*time.Minute, "poll interval")
	recent := fs.Int("recent", model.DefaultAuthorRecent, "follow only the author's N most recently updated PRs (all repos together)")
	paths := fs.String("paths", "", "track only PRs changing a file matching one of these globs (GLOB[,GLOB]; ** spans directories)")
	titleMatch := fs.String("title-match", "", "track only PRs whose title matches this Go regexp (with --paths: either suffices)")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if *login == "" || *repos == "" || *since == "" {
		return fmt.Errorf("--login, --repo and --since are required")
	}
	if *recent < 1 {
		return fmt.Errorf("--recent %d: want at least 1", *recent)
	}
	ref := &model.AuthorRef{Login: *login, AlertRole: *alert, Recent: *recent}
	var err error
	if ref.Since, err = time.Parse(time.RFC3339, *since); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if *until != "" {
		u, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		ref.Until = &u
	}
	for _, r := range strings.Split(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			if !strings.Contains(r, "/") {
				return fmt.Errorf("--repo %q: want owner/repo", r)
			}
			ref.Repos = append(ref.Repos, r)
		}
	}
	var filter model.AuthorFilter
	for _, p := range strings.Split(*paths, ",") {
		if p = strings.TrimSpace(p); p != "" {
			filter.Paths = append(filter.Paths, p)
		}
	}
	filter.TitleMatch = strings.TrimSpace(*titleMatch)
	if err := author.CheckFilter(filter); err != nil {
		return err
	}
	if err := forge.CheckForgeHost(*host); err != nil {
		return err
	}
	if ref.AlertRole != "" && !strings.HasPrefix(ref.AlertRole, "@") {
		return fmt.Errorf("--alert %q: want a role form (@id)", ref.AlertRole)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Mode: model.ModeAuthor, Source: "github-author", Host: *host, CreatedAt: time.Now().UTC(),
		FigaroSock: g.figaro, EverySec: int(every.Seconds()), Author: ref, Stop: model.StopConditions{}}
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
	if keys := filterKeys(filter); len(keys) > 0 {
		if err := st.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
			return keys, nil
		}); err != nil {
			return fmt.Errorf("critic created (role %s), but its filter: %w", w.FormID, err)
		}
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
	fmt.Printf("author watch %q: role %s, %s in %s since %s, alert %s, every %s\n", *name, formID, ref.Login,
		strings.Join(ref.Repos, ","), ref.Since.UTC().Format(time.RFC3339), w.AlertTarget(), *every)
	if len(filter.Paths) > 0 || filter.TitleMatch != "" {
		fmt.Printf("  filter: form keys %s, %s (edit with figaro set; a bad value keeps the last good one, see %s)\n",
			model.AuthorPathsKey, model.AuthorTitleKey, model.AuthorFilterErr)
	}
	return nil
}

// filterKeys are the role-form keys an author filter is stored in.
func filterKeys(f model.AuthorFilter) map[string]json.RawMessage {
	keys := map[string]json.RawMessage{}
	if len(f.Paths) > 0 {
		keys[model.AuthorPathsKey] = model.MustJSON(f.Paths)
	}
	if f.TitleMatch != "" {
		keys[model.AuthorTitleKey] = model.MustJSON(f.TitleMatch)
	}
	return keys
}
