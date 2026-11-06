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

// cmdAddKeel mints a keel-threads watch: role form, critic, optional seat, timer.
func cmdAddKeel(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-keel")
	name := fs.String("name", "", "critic name, e.g. keel-notes")
	repo := fs.String("repo", "", "keel repo, owner/name")
	paths := fs.String("paths", "", "comma-separated path globs (path.Match; dir/** for a subtree); empty = every path")
	aria := fs.String("aria", "", "seat cast into the critic's own role form")
	alert := fs.String("alert", "", "role to wake (@form) instead; its target-aria is resolved at every send")
	waiting := fs.String("waiting-on", "", "comma-separated authors whose answer wakes the seat (name, account or alias)")
	self := fs.String("self", "", "comma-separated authors that are the seat itself: never announced")
	every := fs.Duration("every", 5*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := model.ValidName(*name); err != nil {
		return err
	}
	ref := &model.KeelRef{Repo: strings.TrimSpace(*repo), Paths: splitList(*paths), WaitingOn: splitList(*waiting), Self: splitList(*self), AlertRole: *alert}
	if owner, nm, ok := strings.Cut(ref.Repo, "/"); !ok || owner == "" || nm == "" || strings.Contains(nm, "/") {
		return fmt.Errorf("--repo %q: want owner/name", ref.Repo)
	}
	if len(ref.WaitingOn) == 0 {
		return fmt.Errorf("--waiting-on is required")
	}
	if *aria != "" && *alert != "" {
		return fmt.Errorf("--aria and --alert are exclusive: one seat or role per critic")
	}
	if *alert != "" && !strings.HasPrefix(*alert, "@") {
		return fmt.Errorf("--alert %q: want a role form (@id)", *alert)
	}
	if *aria != "" {
		if err := model.ValidAriaID(*aria); err != nil {
			return fmt.Errorf("--aria: %w", err)
		}
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Mode: model.ModeKeel, Source: "keel-threads", CreatedAt: time.Now().UTC(),
		FigaroSock: g.figaro, EverySec: int(every.Seconds()), Keel: ref, Stop: model.StopConditions{}}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if *aria != "" {
		if err := st.Fig.Cast(ctx, *aria, formID); err != nil {
			return fmt.Errorf("critic created (role %s), but casting %s failed: %w", formID, *aria, err)
		}
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("keel watch %q: role %s, %s paths %q, waiting on %s, alert %s, every %s\n", *name, formID, ref.Repo,
		strings.Join(ref.Paths, ","), strings.Join(ref.WaitingOn, ","), w.AlertTarget(), *every)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
