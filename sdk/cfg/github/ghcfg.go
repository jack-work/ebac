// Package ghcfg is the github plugin's pure configuration: validators and
// compiled forms the core CLI (ebac add-*, ebac set) and the plugin share.
// No I/O.
package ghcfg

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/relevance"
)

// CompileFilter compiles an author watch's relevance filter.
func CompileFilter(spec model.AuthorFilter) (*relevance.Filter, error) {
	return relevance.Compile(spec.TitleMatch, spec.Paths)
}

// CheckFilter validates an author filter before it is stored (ebac add-author).
func CheckFilter(spec model.AuthorFilter) error {
	_, err := CompileFilter(spec)
	return err
}

// Owner is a compiled OwnerRule.
type Owner struct {
	Author string
	Title  *regexp.Regexp
	Role   string
}

var roleRE = regexp.MustCompile(`^@[0-9a-f]{8,32}$`)

func CompileOwners(rules []model.OwnerRule) ([]Owner, error) {
	var out []Owner
	for i, r := range rules {
		o := Owner{Author: r.Author, Role: r.Role}
		if r.Author == "" || !roleRE.MatchString(r.Role) {
			return nil, fmt.Errorf("owner rule %d: want an author and a role @<hex>, got %q -> %q", i, r.Author, r.Role)
		}
		if r.TitleMatch != "" {
			re, err := regexp.Compile(r.TitleMatch)
			if err != nil {
				return nil, fmt.Errorf("owner rule %d: title_match: %w", i, err)
			}
			o.Title = re
		}
		out = append(out, o)
	}
	return out, nil
}

// CheckOwners validates owner rules before they are stored (ebac add-scout).
func CheckOwners(rules []model.OwnerRule) error {
	_, err := CompileOwners(rules)
	return err
}

// OwnerOf is the owner rule of a PR (1-based): the first matching its author (and title), or 0.
func OwnerOf(owners []Owner, author, title string) int {
	for i, o := range owners {
		if strings.EqualFold(o.Author, author) && (o.Title == nil || o.Title.MatchString(title)) {
			return i + 1
		}
	}
	return 0
}

// RoleOf is the owner role of a PR, or "".
func RoleOf(owners []Owner, author, title string) string {
	if i := OwnerOf(owners, author, title); i > 0 {
		return owners[i-1].Role
	}
	return ""
}

// CheckApprove refuses an incomplete configuration.
// askNameRe: the asker goes into a GHE review body verbatim.
var askNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z .'-]{0,59}$`)

func CheckApprove(w *model.ApproveWatch) error {
	switch {
	case w == nil:
		return fmt.Errorf("no approve configuration")
	case !strings.HasPrefix(w.Chat, "19:"):
		return fmt.Errorf("chat %q: want a Teams chat id (19:...)", w.Chat)
	case w.ChatAlias == "":
		return fmt.Errorf("chat alias is required")
	case !askNameRe.MatchString(w.Asker()):
		return fmt.Errorf("ask name %q: want a person's name (letters, space, . ' -; at most 60)", w.Asker())
	case !strings.HasPrefix(w.AskFrom, "8:orgid:"):
		return fmt.Errorf("ask_from %q: want a Teams sender id (8:orgid:<guid>)", w.AskFrom)
	case w.Grant == "" || strings.ContainsAny(w.Grant, "*?"):
		return fmt.Errorf("grant %q: want one exact credential name", w.Grant)
	case w.Principal == "":
		return fmt.Errorf("principal is required")
	case w.PingTo != "" && !strings.HasPrefix(w.PingTo, "@"):
		return fmt.Errorf("ping_to %q: want a role form (@id)", w.PingTo)
	}
	return nil
}

// CheckOrgApprove refuses an incomplete configuration.
func CheckOrgApprove(w *model.OrgApproveWatch) error {
	switch {
	case w == nil:
		return fmt.Errorf("no org-approve configuration")
	case len(w.Logins) == 0:
		return fmt.Errorf("logins: want at least one GHE login")
	case w.Grant == "" || strings.ContainsAny(w.Grant, "*?"):
		return fmt.Errorf("grant %q: want one exact credential name", w.Grant)
	case w.Principal == "":
		return fmt.Errorf("principal is required")
	case w.Approver == "" || strings.HasPrefix(w.Approver, "flake:"):
		return fmt.Errorf("approver %q: want a person (band-grants approved_by)", w.Approver)
	case w.PingTo != "" && !strings.HasPrefix(w.PingTo, "@"):
		return fmt.Errorf("ping_to %q: want a role form (@id)", w.PingTo)
	}
	for _, l := range w.Logins {
		if !ValidLogin(l) {
			return fmt.Errorf("login %q: want a GHE login", l)
		}
	}
	return nil
}

// ValidLogin reports whether s can be a GHE login in a search query.
func ValidLogin(s string) bool { return loginRe.MatchString(s) }

var loginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)

// Host is the forge host of approve.Repo and org-approve.
const Host = "microsoft.ghe.com"
