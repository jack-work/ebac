package main

import (
	"context"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/model"
)

// cmdAddApprove mints an approve-requests critic (GO-20261112-secretary-ahmed):
// no seat; it reads requests on the inputs form and writes their results.
// Its configuration lives on its own role form, minted here, which the
// requester cannot write.
func cmdAddApprove(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-approve")
	name := fs.String("name", "ahmed-approve", "critic name; the role form is ebac-<name>")
	inputs := fs.String("inputs", "", "the requests form (@id): the requester writes requests.<id>, ebac writes requests.<id>.result")
	chat := fs.String("chat", "", "the Teams chat id the ask must be in (19:...)")
	alias := fs.String("chat-alias", "", "the chat value a request must carry (e.g. ahmed)")
	from := fs.String("ask-from", "", "the Teams sender id the ask must come from (8:orgid:<guid>)")
	grant := fs.String("grant", "ebac-approve.aether", "the band-grants credential that must be live for --principal (exact)")
	principal := fs.String("principal", "", "the band-grants principal, as stored in grants.json (required)")
	ping := fs.String("ping", "", "the role (@form) told one line per approve")
	askName := fs.String("ask-name", "", "the asker's name in the review body and ping (default: --chat-alias)")
	every := fs.Duration("every", time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	aw := &model.ApproveWatch{Chat: *chat, ChatAlias: *alias, AskFrom: *from, Grant: *grant, Principal: *principal, PingTo: *ping, AskName: *askName}
	if err := ghcfg.CheckApprove(aw); err != nil {
		return err
	}
	if !strings.HasPrefix(*inputs, "@") {
		return fmt.Errorf("--inputs %q: want a form (@id)", *inputs)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Source: model.SourceApprove, Host: ghcfg.Host, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), InputsForm: *inputs, Requests: aw, Stop: model.StopConditions{}}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.CreateCritic(ctx, w); err != nil {
		return err
	}
	fmt.Printf("approve-requests %q: role %s, requests %s, %s for %s, ask in %s from %s, every %s\n",
		*name, formID, *inputs, aw.Grant, aw.Principal, aw.ChatAlias, aw.AskFrom, *every)
	return nil
}
