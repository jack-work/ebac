package main

import (
	"context"
	"fmt"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/model"
)

// cmdConfig is `ebac config check`: the declared critics (services.ebac ->
// /etc/ebac/config.json registrations) against the live ones, one line per
// difference; none = the configuration generates exactly the live set
// (rework step 3's proof). Exit status 1 when they differ.
func cmdConfig(ctx context.Context, g globals, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("usage: ebac config check")
	}
	regs, declared := form.Registrations()
	if !declared {
		return fmt.Errorf("%s declares no registrations", form.TrustedPath)
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	ws, err := st.ListCritics(ctx)
	if err != nil {
		return err
	}
	live := map[string]model.Registration{}
	for _, w := range ws {
		if w.Runtime.Archived {
			continue
		}
		f, err := st.ReadForm(ctx, w.FormID)
		if err != nil {
			return fmt.Errorf("%s: %w", w.Name, err)
		}
		live[w.Name] = model.Registration{Form: w.FormID, Critic: f.Keys[model.CriticKey]}
	}
	diff := model.CheckRegistrations(regs, live)
	for _, l := range diff {
		fmt.Println(l)
	}
	if len(diff) > 0 {
		return fmt.Errorf("%d difference(s) between the configuration and the live critics", len(diff))
	}
	fmt.Printf("configuration == live: %d critic(s)\n", len(live))
	return nil
}
