package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/source/approve"
	"github.com/gluck/ebac/internal/source/orgapprove"
	"github.com/gluck/ebac/internal/systemd"
)

// cmdAddOrgApprove mints an org-approve critic (ORG-APPROVE, the Boss
// 2026-11-17): no seat; its inputs form is the elicitation board it asks on.
func cmdAddOrgApprove(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-org-approve")
	name := fs.String("name", "org-approve", "critic name; the role form is ebac-<name>")
	board := fs.String("board", "", "the elicitation board (@id) the asks go on as q.oa-<pr>-<head7>")
	logins := fs.String("logins", "", "the org's GHE logins, comma-separated")
	grant := fs.String("grant", "ebac-approve.org", "the band-grants credential each tap request is filed for (exact)")
	principal := fs.String("principal", "org-approve", "ebac's band-grants principal on the request socket")
	approver := fs.String("approver", "jack", "the only approved_by a tap is taken from")
	ping := fs.String("ping", "", "the role (@form) told one line per ask and approval")
	breaks := fs.String("known-breaks", "", "check names allowed red (main-wide known breaks), comma-separated")
	every := fs.Duration("every", 15*time.Minute, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	ow := &model.OrgApproveWatch{Logins: splitList(*logins), Grant: *grant, Principal: *principal, Approver: *approver,
		PingTo: *ping, KnownBreaks: splitList(*breaks)}
	if err := orgapprove.CheckWatch(ow); err != nil {
		return err
	}
	if !strings.HasPrefix(*board, "@") {
		return fmt.Errorf("--board %q: want a form (@id)", *board)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Source: model.SourceOrgApprove, Host: approve.Host, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		EverySec: int(every.Seconds()), InputsForm: *board, OrgApprove: ow, Stop: model.StopConditions{}}
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
	fmt.Printf("org-approve %q: role %s, board %s, %d login(s), taps = %s for %s approved by %s, every %s\n",
		*name, formID, *board, len(ow.Logins), ow.Grant, ow.Principal, ow.Approver, *every)
	return nil
}

// cmdGrantRequest files one band-grants request as ebac's principal (the
// socket maps the caller's uid) and prints its id.
func cmdGrantRequest(ctx context.Context, args []string) error {
	fs := newFlagSet("grant-request")
	cred := fs.String("credential", "", "the credential asked for (exact)")
	ttlS := fs.String("ttl", "", "how long: a Go duration or whole days, e.g. 2h or 30d")
	principal := fs.String("principal", "", "the principal it is filed as (the socket maps the caller's uid; this only labels the output)")
	reason := fs.String("reason", "", "why, 1..300 characters (shown on /grants/)")
	sock := fs.String("socket", approve.RequestSocket, "band-grants' request socket")
	_ = fs.Parse(args)
	ttl, err := parseDays(*ttlS)
	if err != nil {
		return fmt.Errorf("--ttl %q: %w", *ttlS, err)
	}
	switch {
	case *cred == "" || strings.ContainsAny(*cred, "*? "):
		return fmt.Errorf("--credential %q: want one exact credential name", *cred)
	case ttl < time.Second:
		return fmt.Errorf("--ttl %s: want at least 1s", ttl)
	case len(*reason) == 0 || len(*reason) > 300:
		return fmt.Errorf("--reason: want 1..300 characters")
	}
	id, err := approve.Filer{Socket: *sock}.File(ctx, *cred, ttl, *reason)
	if err != nil {
		return err
	}
	if *principal != "" {
		fmt.Printf("%s (%s for %s, %s: approve on %s)\n", id, *cred, *principal, ttl, orgapprove.GrantsPage)
		return nil
	}
	fmt.Println(id)
	return nil
}

// parseDays is time.ParseDuration plus whole days ("30d").
func parseDays(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("want whole days, e.g. 30d")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
