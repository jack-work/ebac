package approve

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GrantsFile is band-grants' published list of LIVE grants (0644): a revoked
// grant is removed from it, a pending or denied request never appears in it.
// band-cred reads the same file at each use.
const GrantsFile = "/var/lib/band-grants/grants.json"

// Grant is one row of GrantsFile, as band-grants writes it.
type Grant struct {
	ID         string `json:"id"`
	Principal  string `json:"principal"`
	Credential string `json:"credential"`
	ExpiresAt  int64  `json:"expires_at"`
	ApprovedBy string `json:"approved_by"`
}

// ReadGrants reads path. A missing or unreadable file is an error: the
// caller refuses, never assumes a grant.
func ReadGrants(path string) ([]Grant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Grants []Grant `json:"grants"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Grants, nil
}

// liveGrant is the reason the grant check fails, or "" when a grant for
// exactly principal and credential is live at now (unix s) and was decided
// by a person, not a standing flake approval (flake:<owner>): the Boss
// approves this scope on /grants/, never the flake.
func liveGrant(gs []Grant, principal, credential string, now int64) string {
	why := fmt.Sprintf("no live grant %s for %s", credential, principal)
	for _, g := range gs {
		if g.Principal != principal || g.Credential != credential {
			continue
		}
		switch {
		case g.ExpiresAt <= now:
			why = fmt.Sprintf("grant %s for %s expired at %d", credential, principal, g.ExpiresAt)
		case strings.HasPrefix(g.ApprovedBy, "flake:") || g.ApprovedBy == "":
			why = fmt.Sprintf("grant %s for %s is standing (%q), not a Boss approval", credential, principal, g.ApprovedBy)
		default:
			return ""
		}
	}
	return why
}
