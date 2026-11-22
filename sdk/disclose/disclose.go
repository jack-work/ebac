package disclose

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// GHE text may name figaro and no other internal tool, role, node or path
// (Boss order 3, 2026-11-16). disclose_names.txt is a copy of the GHE audit's
// list (~/notes/captains-log/cheroot/secretary/ghe-audit/names.txt): one
// case-insensitive regex per line, # comments. The figaro line is skipped.
//
//go:embed disclose_names.txt
var discloseNames string

type discloseTerm struct {
	src string
	re  *regexp.Regexp
}

// EnvList names a file that REPLACES the embedded list: the deployment's one
// list for every GHE text (the package wrapper sets it, so the daemon, the CLI
// and every plugin read the same file). Set but unreadable, empty or invalid:
// the process does not start (fail closed). Unset: the embedded list.
const EnvList = "EBAC_DISCLOSE_NAMES"

var discloseTerms = mustTerms(loadTerms(os.Getenv, os.ReadFile))

func loadTerms(getenv func(string) string, read func(string) ([]byte, error)) ([]discloseTerm, error) {
	p := getenv(EnvList)
	if p == "" {
		return parseDisclose(discloseNames), nil
	}
	b, err := read(p)
	if err != nil {
		return nil, fmt.Errorf("%s=%s: %v", EnvList, p, err)
	}
	ts := parseDisclose(string(b))
	if len(ts) == 0 {
		return nil, fmt.Errorf("%s=%s: no terms", EnvList, p)
	}
	return ts, nil
}

func mustTerms(ts []discloseTerm, err error) []discloseTerm {
	if err != nil {
		panic("disclose: " + err.Error())
	}
	return ts
}

// parseDisclose compiles the list. RE2 has no lookahead, so (?= becomes (?:,
// which matches the same bodies (only the span differs).
func parseDisclose(list string) []discloseTerm {
	var out []discloseTerm
	for _, ln := range strings.Split(list, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || ln == `\bfigaro\b` {
			continue
		}
		re, err := regexp.Compile("(?i)" + strings.ReplaceAll(ln, "(?=", "(?:"))
		if err != nil {
			panic(fmt.Sprintf("disclose_names.txt: %q: %v", ln, err))
		}
		out = append(out, discloseTerm{ln, re})
	}
	return out
}

// Disclosure returns why body may not go to GHE ("" when it is clean).
func Disclosure(body string) string {
	for _, t := range discloseTerms {
		if m := t.re.FindString(body); m != "" {
			return fmt.Sprintf("body names an internal term %q (rule %s): GHE text may name figaro and nothing else internal; rephrase in broadly known terms", m, t.src)
		}
	}
	return ""
}
