package main

import (
	"context"
	"fmt"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/form"
)

// cmdRepoint rewrites the role-form addresses critics store (role-hierarchy
// migration, step 1).
func cmdRepoint(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("repoint")
	critic := fs.String("critic", "", "the critic whose alert role to set (with --alert)")
	alert := fs.String("alert", "", "the new alert role (@form id or unique prefix)")
	from := fs.String("all-from", "", "rewrite every role address equal to this form (@id or unique prefix) in every critic (with --to)")
	to := fs.String("to", "", "the new role (@form id or unique prefix)")
	dry := fs.Bool("dry-run", false, "print critic, field, old -> new; write nothing")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `ebac repoint --critic N --alert @new [--dry-run]
ebac repoint --all-from @old --to @new [--dry-run]

Rewrites the role-form addresses a critic's configuration stores: alert_role (author, keel, releases watches),
the scout's owner roles and warn role, keel-branches' reviewer. Old matches by full id or unique prefix (a stored
8+ hex prefix of it matches too); the full new id is written. The new role must be a role form with a holder
(target-aria). Idempotent: a second run changes nothing. Each critic key is rewritten whole through the Commit guard.

Not here: a critic's SEAT, the holder (target-aria) of its own role form: that is ebac cast --critic N --aria <id>.
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fig := figaro.NewFigaro(g.figaro)
	st, err := form.NewStore(fig)
	if err != nil {
		return err
	}
	forms, err := fleet.ListForms(ctx, fig)
	if err != nil {
		return err
	}
	changes, err := fleet.Repoint(ctx, st, forms, fleet.RepointReq{Critic: *critic, Alert: *alert, From: *from, To: *to, DryRun: *dry})
	verb := "repointed"
	if *dry {
		verb = "would repoint"
	}
	for _, c := range changes {
		fmt.Printf("%s %s %s -> %s\n", c.Critic, c.Field, c.Old, c.New)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s %d address(es)\n", verb, len(changes))
	return nil
}
