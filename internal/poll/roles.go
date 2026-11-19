package poll

import (
	"context"
	"fmt"
	"strings"
)

// RosterCritic is the roster critic whose form lists every role, one key
// per role: role:<name> = "@<form> -> <holder> ...".
const RosterCritic = "roles"

// RoleNames maps each role name on the roster form to its role form (@id).
// Moved-away names ("<name>~moved") are skipped: their forms forward only for
// a while.
func (p *Poller) RoleNames(ctx context.Context) (map[string]string, error) {
	c, err := p.Store.LoadCritic(ctx, RosterCritic)
	if err != nil {
		return nil, fmt.Errorf("roster critic %q: %w", RosterCritic, err)
	}
	f, err := p.Fig.Form(ctx, c.FormID)
	if err != nil {
		return nil, fmt.Errorf("roster form %s: %w", c.FormID, err)
	}
	return ParseRoster(f), nil
}

// ParseRoster reads role:<name> keys ("@<form> -> ...") into name -> @form.
func ParseRoster(f map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range f {
		name, ok := strings.CutPrefix(k, "role:")
		if !ok || name == "" || strings.Contains(name, "~") {
			continue
		}
		name = strings.NewReplacer("%2E", ".", "%2A", "*").Replace(name)
		line, _ := v.(string)
		id, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if !strings.HasPrefix(id, "@") || len(id) < 2 {
			continue
		}
		out[name] = id
	}
	return out
}
