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

// cmdAddTeams mints a teams critic: role form, critic, seat, timer.
func cmdAddTeams(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-teams")
	name := fs.String("name", "", "critic name, e.g. teams-md1-cowork")
	chat := fs.String("chat", "", "Teams conversation id (19:...@thread.v2)")
	topic := fs.String("topic", "", "chat topic, used to find it again")
	aria := fs.String("aria", "", "seat: the aria cast into the role")
	quiet := fs.String("quiet", "", "comma-separated senders that never wake the seat: display names or sender ids (8:orgid:<guid> or the bare guid)")
	every := fs.Duration("every", 60*time.Second, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if *chat == "" {
		return fmt.Errorf("--chat is required")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Mode: model.ModeTeams, Source: "teams-chat", CreatedAt: time.Now().UTC(), FigaroSock: g.figaro, EverySec: int(every.Seconds()),
		Chat: &model.ChatRef{ID: *chat, Topic: *topic}, Stop: model.StopConditions{}}
	for _, q := range strings.Split(*quiet, ",") {
		if q = strings.TrimSpace(q); q != "" {
			w.Chat.Quiet = append(w.Chat.Quiet, q)
		}
	}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
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
	fmt.Printf("teams critic %q: role %s, chat %s (%s), seat %s, every %s\n", *name, formID, *chat, *topic, *aria, *every)
	return nil
}
