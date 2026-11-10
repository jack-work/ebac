// Package relevance is the one PR relevance filter (cheroot work order
// 2026-11-09): a title regex and path globs. The author watch and the scout
// both use it; a PR is relevant on a title hit or when any file it changes
// matches a glob.
package relevance

import (
	"fmt"
	"regexp"
	"strings"
)

// Filter is a compiled filter; nil (or an empty one) matches everything.
type Filter struct {
	title *regexp.Regexp
	paths []*regexp.Regexp
}

// Compile compiles a title regex ("" unset) and path globs (see Glob). Errors
// name the key they came from: title_match or paths.
func Compile(titleMatch string, paths []string) (*Filter, error) {
	f := &Filter{}
	if titleMatch != "" {
		re, err := regexp.Compile(titleMatch)
		if err != nil {
			return nil, fmt.Errorf("title_match: %w", err)
		}
		f.title = re
	}
	for _, g := range paths {
		re, err := Glob(g)
		if err != nil {
			return nil, fmt.Errorf("paths: %w", err)
		}
		f.paths = append(f.paths, re)
	}
	return f, nil
}

// All says the filter is unset: every PR is relevant.
func (f *Filter) All() bool { return f == nil || (f.title == nil && len(f.paths) == 0) }

// HasPaths says whether file lists can decide anything.
func (f *Filter) HasPaths() bool { return f != nil && len(f.paths) > 0 }

// TitleHit says whether the title matches the title regex (false when unset).
func (f *Filter) TitleHit(title string) bool {
	return f != nil && f.title != nil && f.title.MatchString(title)
}

// PathHit says whether any file matches a glob. A glob matches the file's path
// or any directory above it, so "**/system1*" catches a file under system1/.
func (f *Filter) PathHit(files []string) bool {
	if f == nil {
		return false
	}
	for _, file := range files {
		for p := file; p != ""; {
			for _, re := range f.paths {
				if re.MatchString(p) {
					return true
				}
			}
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return false
}

// Glob compiles a path glob: ** spans directories ("**/" also matches none),
// * and ? stay within one. Brackets and braces are refused, so a glob never
// means something other than it reads.
func Glob(g string) (*regexp.Regexp, error) {
	if strings.ContainsAny(g, "[]{}\\") {
		return nil, fmt.Errorf("glob %q: brackets, braces and backslashes are not supported", g)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i += 2
		case g[i] == '*':
			b.WriteString("[^/]*")
			i++
		case g[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(g[i : i+1]))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
