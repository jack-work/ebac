// Package identity decides whether an aria may act for a role.
//
// A role resolves by NAME over one listing of every live form: exactly one
// live form named <role> ("<role>~moved" forms excluded); none or several is
// refused (a second form with the same name never competes). A VERDICT may
// come only from the role's holder (the role form's target-aria); a POST from
// the holder or a live fork of it.
//
// Its limit: the caller's aria is a CLAIM. Every seat reaches ebac as the
// same uid through the same door, so ebac cannot tell which aria called it;
// this stops acting for a role by mistake and ties every stamped write to the
// role form and holder that were checked, but a seat that names another aria
// is not stopped until figaro attests the writer of a form patch (figaro#139).
package identity

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MaxHops bounds every fork chain walked: far above the deepest live chain
// (29 measured 2026-11-19); a loop is caught by the visited set, not by this.
const MaxHops = 256

var ariaID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Directory is read fresh for each round or command, never across them.
type Directory struct {
	// Forms maps a form name to the full ids (@<32 hex>) of the live forms
	// carrying it.
	Forms map[string][]string
	// Holder maps every live form's full id to its target-aria.
	Holder map[string]string
	// Parent maps every listed aria, reaped ones included, to its parent
	// ("" at a root; a form id is a root too): a fork chain walks through
	// reaped ancestors.
	Parent map[string]string
	// Reaped marks listed arias the store has reaped: never a live caller.
	Reaped map[string]bool
}

// Resolve is the one live role form named role.
func (d Directory) Resolve(role string) (form, why string) {
	fs := append([]string(nil), d.Forms[role]...)
	sort.Strings(fs)
	switch len(fs) {
	case 0:
		return "", fmt.Sprintf("role %q: no live role form of that name", role)
	case 1:
		return fs[0], ""
	}
	return "", fmt.Sprintf("role %q: %d live forms carry that name (%s): ambiguous, refused", role, len(fs), strings.Join(fs, ", "))
}

// live is "" when a is a live aria, else why not.
func (d Directory) live(a string) string {
	if d.Reaped[a] {
		return "is reaped"
	}
	if _, ok := d.Parent[a]; ok {
		return ""
	}
	return "is not a live aria"
}

func (d Directory) check(caller, role string, forks bool) (form string, ok bool, why string) {
	if !ariaID.MatchString(caller) {
		return "", false, fmt.Sprintf("caller %q: want a full 32-hex aria id", caller)
	}
	if w := d.live(caller); w != "" {
		return "", false, fmt.Sprintf("caller %s %s", caller, w)
	}
	form, why = d.Resolve(role)
	if why != "" {
		return "", false, why
	}
	holder := d.Holder[form]
	if holder == "" {
		return form, false, fmt.Sprintf("role %s (%s) has no seat cast", role, form)
	}
	if caller == holder {
		return form, true, ""
	}
	if !forks {
		return form, false, fmt.Sprintf("caller %s is not the holder of role %s (%s is held by %s)", caller, role, form, holder)
	}
	switch d.descends(caller, holder) {
	case yes:
		return form, true, ""
	case no:
		return form, false, fmt.Sprintf("caller %s does not hold role %s and is not a live fork of its holder %s", caller, role, holder)
	}
	return form, false, fmt.Sprintf("caller %s: its fork chain to %s's holder %s cannot be established (an unlisted link, a loop, or more than %d forks)", caller, role, holder, MaxHops)
}

// MayVerdict: only the role's holder records a verdict.
func (d Directory) MayVerdict(caller, role string) (form string, ok bool, why string) {
	return d.check(caller, role, false)
}

// MayPost: the role's holder or a live fork of it writes for the role.
func (d Directory) MayPost(caller, role string) (form string, ok bool, why string) {
	return d.check(caller, role, true)
}

type answer int

const (
	no answer = iota
	yes
	unknown // a reaped link, a loop or the hop bound: neither yes nor no
)

// descends walks parent links from a; yes when it reaches anc, no when it
// reaches a root, unknown when the chain cannot be followed to an end.
func (d Directory) descends(a, anc string) answer {
	seen := map[string]bool{}
	for hops := 0; hops <= MaxHops; hops++ {
		if a == anc {
			return yes
		}
		if seen[a] {
			return unknown
		}
		seen[a] = true
		p, ok := d.Parent[a]
		if !ok {
			return unknown // an unlisted link (reaped ones stay listed)
		}
		if p == "" || strings.HasPrefix(p, "@") {
			return no
		}
		a = p
	}
	return unknown
}

// Independent says caller is neither aria nor a fork of it, established; a
// chain that cannot be followed is NOT independent (fails closed).
func (d Directory) Independent(caller, aria string) (bool, string) {
	if aria == "" {
		return false, "the author side has no holder"
	}
	switch d.descends(caller, aria) {
	case no:
		return true, ""
	case yes:
		return false, fmt.Sprintf("caller %s is the author side (%s or a fork of it)", caller, aria)
	}
	return false, fmt.Sprintf("caller %s: independence from the author side %s cannot be established", caller, aria)
}
