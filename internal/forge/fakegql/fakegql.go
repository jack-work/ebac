// Package fakegql answers the aliased GraphQL reads forge sends, for the fake
// gh of tests: a PR read from one PR's document as the old single-PR query
// returned it, a search from `gh pr list --json` rows. It imports nothing of
// ebac, so forge's own tests and forgetest can both use it.
package fakegql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	prAlias     = regexp.MustCompile(`(p\d+): repository\(owner:"[^"]*",name:"[^"]*"\)\{ pullRequest\(number:(\d+)\)\{`)
	reviewsFrom = regexp.MustCompile(`reviews\(first:50, after:("[^"]*")\)`)
	searchAlias = regexp.MustCompile(`(?m)^\s*(s\d+): search\(`)
)

// Query is the query text of a gh argv ("" when it has none).
func Query(args []string) string {
	for _, a := range args {
		if q, ok := strings.CutPrefix(a, "query="); ok {
			return q
		}
	}
	return ""
}

// IsSearch says whether a query is an aliased search (an author list).
func IsSearch(q string) bool { return searchAlias.MatchString(q) }

// PRs answers an aliased PR read: doc(number, reviewsCursor) is one PR's
// {"data":{"repository":{"pullRequest":...}}} (or {"errors":[...]}, which
// becomes that alias's error). The selection after the alias line holds the
// reviews cursor (forge.aliasedPRs).
func PRs(q string, doc func(number, reviewsCursor string) string) string {
	data := map[string]any{"rateLimit": map[string]int{"cost": 1, "remaining": 4999}}
	var errs []any
	at := prAlias.FindAllStringSubmatchIndex(q, -1)
	for i, ix := range at {
		end := len(q)
		if i+1 < len(at) {
			end = at[i+1][0]
		}
		m := []string{q[ix[0]:ix[1]], q[ix[2]:ix[3]], q[ix[4]:ix[5]], q[ix[1]:end]}
		cursor := ""
		if c := reviewsFrom.FindStringSubmatch(m[3]); c != nil {
			cursor, _ = strconv.Unquote(c[1])
		}
		var one struct {
			Data struct {
				Repository struct {
					PullRequest json.RawMessage `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []map[string]any `json:"errors"`
		}
		if err := json.Unmarshal([]byte(doc(m[2], cursor)), &one); err != nil {
			panic(fmt.Sprintf("fakegql: PR %s document: %v", m[2], err))
		}
		for _, e := range one.Errors {
			e["path"] = []any{m[1], "pullRequest"}
			errs = append(errs, e)
		}
		if len(one.Errors) == 0 {
			data[m[1]] = map[string]json.RawMessage{"pullRequest": one.Data.Repository.PullRequest}
		} else {
			data[m[1]] = map[string]any{"pullRequest": nil}
		}
	}
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Search answers every search alias of q with the same rows (a `gh pr list
// --json` array, as the fake gh served the two list calls before them), one
// page.
func Search(q, rows string) string {
	var nodes []map[string]any
	if err := json.Unmarshal([]byte(rows), &nodes); err != nil {
		panic(fmt.Sprintf("fakegql: rows: %v", err))
	}
	for _, n := range nodes {
		for _, k := range []string{"mergedAt", "closedAt"} {
			if s, _ := n[k].(string); strings.HasPrefix(s, "0001-") {
				n[k] = nil // gh pr list's zero time is GraphQL's null
			}
		}
	}
	data := map[string]any{"rateLimit": map[string]int{"cost": 1, "remaining": 4999}}
	for _, m := range searchAlias.FindAllStringSubmatch(q, -1) {
		data[m[1]] = map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes}
	}
	b, _ := json.Marshal(map[string]any{"data": data})
	return string(b)
}
