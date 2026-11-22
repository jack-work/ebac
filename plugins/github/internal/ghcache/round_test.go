package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/forge"
)

// fakeGH answers the queries this package sends from a world of PRs (production-shaped: the captured deep reads),
// counts calls, and fails on demand. Searches answer by their query string.
type fakeGH struct {
	t        *testing.T
	prs      map[string]map[int]*PR // repo -> n -> PR
	search   map[string][]int       // search query -> numbers (looked up in the search's repo)
	calls    []string               // every GraphQL query
	lsrefs   int
	failGQL  func(q string) error
	failRefs error
	// The core's side: the children it holds and how many of the last reply's writes landed; failLand is the index
	// of the write that fails to land (-1: none).
	children map[string]string
	landed   int
	failLand int
	// searchTotal, when set, is every search's issueCount (a search past SearchFirst).
	searchTotal int
	// wr answers the write path's pre-write reads and mutations (sync_test.go); nil: none expected.
	wr *fakeWrites
}

var (
	searchAliasRe = regexp.MustCompile(`(s\d+): search\(query:"((?:[^"\\]|\\.)*)"`)
	repoAliasRe   = regexp.MustCompile(`(r\d+): repository\(owner:"([^"]+)",name:"([^"]+)"\)\{`)
	prAliasRe     = regexp.MustCompile(`p(\d+): pullRequest\(number:(\d+)\)\{(\.\.\.D|number updatedAt)`)
)

// GraphQLPartial answers as GHE does: a missing PR is null data plus a NOT_FOUND error on [rN, pN] (measured on
// bic/aether, 2026-11-20; forge's TestGraphQLPartialKeepsTheAnsweredAliases pins the decoding).
func (f *fakeGH) GraphQLPartial(_ context.Context, q string) (json.RawMessage, []forge.GQLError, error) {
	f.calls = append(f.calls, q)
	if f.failGQL != nil {
		if err := f.failGQL(q); err != nil {
			return nil, nil, err
		}
	}
	if strings.HasPrefix(q, "{ pre: node(") {
		return f.preWrite(q)
	}
	var errs []forge.GQLError
	out := map[string]any{}
	for _, m := range searchAliasRe.FindAllStringSubmatch(q, -1) {
		query := strings.ReplaceAll(m[2], `\"`, `"`)
		repo := strings.TrimPrefix(strings.Fields(query)[0], "repo:")
		var nodes []any
		for _, n := range f.search[query] {
			nodes = append(nodes, markOf(f.prs[repo][n]))
		}
		total := len(nodes)
		if f.searchTotal > 0 {
			total = f.searchTotal
		}
		out[m[1]] = map[string]any{"issueCount": total, "nodes": nodes}
	}
	locs := repoAliasRe.FindAllStringSubmatchIndex(q, -1)
	for i, loc := range locs {
		alias, repo := q[loc[2]:loc[3]], q[loc[4]:loc[5]]+"/"+q[loc[6]:loc[7]]
		end := len(q)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		prs := map[string]any{}
		for _, m := range prAliasRe.FindAllStringSubmatch(q[loc[1]:end], -1) {
			n, _ := strconv.Atoi(m[2])
			pr := f.prs[repo][n]
			if pr == nil {
				prs["p"+m[1]] = nil
				errs = append(errs, forge.GQLError{Type: "NOT_FOUND", Path: []string{alias, "p" + m[1]},
					Message: "Could not resolve to a PullRequest with the number of " + m[2] + "."})
				continue
			}
			if m[3] == "...D" {
				prs["p"+m[1]] = pr
				if f.wr != nil && f.wr.staleDeep[n] != nil {
					prs["p"+m[1]] = f.wr.staleDeep[n] // the cache's answer lags GitHub (sync_test.go)
				}
			} else {
				prs["p"+m[1]] = markOf(pr)
			}
		}
		out[alias] = prs
	}
	data, err := json.Marshal(out)
	return data, errs, err
}

func markOf(pr *PR) map[string]any {
	m := pr.Mark()
	return map[string]any{"number": pr.Number, "updatedAt": m.UpdatedAt, "headRefOid": m.Head, "state": m.State, "isDraft": m.Draft,
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": m.Rollup}}}}}}
}

func (f *fakeGH) PullHeads(_ context.Context, repo string, prs []int) (map[int]string, error) {
	f.lsrefs++
	if f.failRefs != nil {
		return nil, f.failRefs
	}
	out := map[int]string{}
	for _, n := range prs {
		if pr := f.prs[repo][n]; pr != nil {
			out[n] = pr.Head
		}
	}
	return out, nil
}

func (f *fakeGH) Spent() forge.Spend {
	return forge.Spend{All: len(f.calls) + f.lsrefs, Calls: len(f.calls), Points: len(f.calls), Remaining: 4000}
}

// world: three fixture PRs in bic/aether, numbers kept; search author:John-Kelliher answers 116018 and 127740.
func world(t *testing.T) *fakeGH {
	prs := fixturePRs(t)
	// 116018 was merged at the capture: open it again here so both search hits are live PRs.
	prs[116018].State, prs[116018].Mergeable, prs[116018].MergeSt = "OPEN", "MERGEABLE", "BLOCKED"
	w := &fakeGH{t: t, failLand: -1, children: map[string]string{}, prs: map[string]map[int]*PR{"bic/aether": {116018: prs[116018], 127740: prs[127740], 131611: prs[131611]}},
		search: map[string][]int{"repo:bic/aether is:pr is:open author:John-Kelliher": {116018, 127740}}}
	return w
}

func cfg(t *testing.T, raw string) (*Config, time.Duration) {
	t.Helper()
	c, ttl, err := ParseConfig(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c, ttl
}

var t0 = time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)

func round(t *testing.T, gh *fakeGH, st State, c *Config, ttl time.Duration, at time.Time) (Result, int) {
	t.Helper()
	before := len(gh.calls)
	kids := map[string]string{}
	for k, v := range gh.children {
		kids[k] = v
	}
	r, err := Run(context.Background(), gh, st, Pass{Config: c, ClosedTTL: ttl, Now: at, Children: kids, Landed: gh.landed})
	if err != nil {
		t.Fatalf("round at %s: %v", at, err)
	}
	// The core lands the writes in order and stops at the first failure (landForms); a refused reply (past
	// MaxChildren) lands nothing.
	minted := len(gh.children)
	for _, f := range r.Forms {
		if _, ok := gh.children[f.Key]; f.Retire && ok {
			minted--
		} else if !f.Retire && !ok {
			minted++
		}
	}
	if minted > 100 {
		t.Fatalf("round at %s: %d children: the core would refuse the reply", at, minted)
	}
	gh.landed = 0
	for i, f := range r.Forms {
		if i == gh.failLand {
			break
		}
		if f.Retire {
			delete(gh.children, f.Key)
		} else {
			gh.children[f.Key] = "@" + f.Key
		}
		gh.landed++
	}
	// Each round's state goes through JSON, as the core stores it.
	var back State
	if err := json.Unmarshal(mustJSON(r.State), &back); err != nil {
		t.Fatal(err)
	}
	r.State = back
	return r, len(gh.calls) - before
}

func keys(r Result) string {
	var ks []string
	for _, f := range r.Forms {
		if f.Retire {
			ks = append(ks, "retire:"+f.Key)
		} else {
			ks = append(ks, f.Key)
		}
	}
	return strings.Join(ks, ",")
}

const hotAuthor = `{"repos":["bic/aether"],"rules":[{"author":["John-Kelliher"]}]}`

// The round's cost shape on one hot rule: the first round is one marks call and one deep call writing both PRs; a
// round inside the tier reads ls-refs only (0 GraphQL); the next due round is one marks call and nothing deep when no
// mark moved. RED if the tier gate goes (calls at +1 min) or deep-on-move goes (a deep read at +5 min).
func TestRoundReadsDeepOnlyOnMove(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 2 || keys(r) != "aether-pr-116018,aether-pr-127740" {
		t.Fatalf("first round: %d calls, writes %q; want 2, both", calls, keys(r))
	}
	f := r.Forms[0]
	var g GH
	if err := json.Unmarshal(f.Set["gh"], &g); err != nil || g.Number != 116018 || g.Head != "56be9b2414f0cb6edafebcddb8db624fbbcfc34a" || g.Author != "john-kelliher" {
		t.Fatalf("gh: %+v %v", g, err)
	}
	if len(f.Set) != 3 || f.Set["parsed"] == nil || f.Set["sync"] == nil {
		t.Fatalf("form keys: %v", f.Set)
	}
	if tr := r.Track["bic/aether#116018"]; tr != (Track{Form: "aether-pr-116018", Rule: 0, Tier: "hot", Since: "2026-11-20T04:00:00Z", State: "OPEN"}) {
		t.Fatalf("track: %+v", tr)
	}
	r, calls = round(t, gh, r.State, c, ttl, t0.Add(time.Minute))
	if calls != 0 || len(r.Forms) != 0 {
		t.Fatalf("inside the tier: %d calls, writes %q; want 0, none", calls, keys(r))
	}
	r, calls = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if calls != 1 || len(r.Forms) != 0 || !strings.Contains(gh.calls[len(gh.calls)-1], "s0: search(") {
		t.Fatalf("due, nothing moved: %d calls, writes %q; want 1 marks call, none", calls, keys(r))
	}
	// One PR's updatedAt moves: one marks call, one deep read of that PR only, one write.
	gh.prs["bic/aether"][127740].UpdatedAt = "2026-11-20T04:09:00Z"
	r, calls = round(t, gh, r.State, c, ttl, t0.Add(2*HotEvery))
	last := gh.calls[len(gh.calls)-1]
	if calls != 2 || keys(r) != "aether-pr-127740" || !strings.Contains(last, "p127740: pullRequest(number:127740){...D}") || strings.Contains(last, "p116018") {
		t.Fatalf("one moved: %d calls, writes %q, deep %q", calls, keys(r), last[:120])
	}
}

// The boundary: a hot PR is due at exactly HotEvery (5 min), not a millisecond before. RED if HotEvery changes.
func TestHotTierIsFiveMinutes(t *testing.T) {
	if HotEvery != 5*time.Minute || WarmEvery != 30*time.Minute || ColdEvery != 6*time.Hour {
		t.Fatalf("tiers %v %v %v", HotEvery, WarmEvery, ColdEvery)
	}
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(5*time.Minute-time.Millisecond))
	if calls != 0 {
		t.Fatalf("1 ms early: %d calls", calls)
	}
	if _, calls := round(t, gh, r.State, c, ttl, t0.Add(5*time.Minute)); calls != 1 {
		t.Fatalf("at 5 min: %d calls", calls)
	}
}

// A warm rule searches every 30 min, not every 5. RED if the search ignores the rule's tier.
func TestWarmRuleSearchesEveryThirtyMinutes(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"author":["John-Kelliher"],"tier":"warm"}]}`)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if r.Track["bic/aether#127740"].Tier != "warm" {
		t.Fatalf("tier: %+v", r.Track)
	}
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(29*time.Minute))
	if calls != 0 {
		t.Fatalf("29 min: %d calls", calls)
	}
	if _, calls := round(t, gh, r.State, c, ttl, t0.Add(30*time.Minute)); calls != 1 {
		t.Fatalf("30 min: %d calls", calls)
	}
}

// A head that moves between marks reads is read in full on the next round, from ls-refs alone (no marks call). RED if
// the ls-refs step goes (0 calls until the tier is due).
func TestAMovedHeadIsReadAtOnce(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	gh.prs["bic/aether"][116018].Head = "1111111111111111111111111111111111111111"
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(time.Minute))
	if calls != 1 || keys(r) != "aether-pr-116018" || !strings.Contains(gh.calls[len(gh.calls)-1], "...D") {
		t.Fatalf("moved head: %d calls, writes %q", calls, keys(r))
	}
	// ls-refs failing is not a failed round: the marks decide.
	gh.failRefs = fmt.Errorf("HTTP 502")
	gh.prs["bic/aether"][116018].Head = "2222222222222222222222222222222222222222"
	r, calls = round(t, gh, r.State, c, ttl, t0.Add(2*time.Minute))
	if calls != 0 || !strings.Contains(strings.Join(r.Log, "\n"), "ls-refs bic/aether: HTTP 502 (marks decide this round)") {
		t.Fatalf("ls-refs down: %d calls, log %q", calls, r.Log)
	}
}

// A merged PR leaves the search; its next due marks read shows MERGED and it is read once more, kept ClosedTTL
// (config ttl.closed), then retired and dropped from track. RED if the retire or the TTL goes.
func TestAMergedPRRetiresAfterTheTTL(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"author":["John-Kelliher"]}],"ttl":{"closed":"1h"}}`)
	if ttl != time.Hour {
		t.Fatalf("ttl %v", ttl)
	}
	r, _ := round(t, gh, State{}, c, ttl, t0)
	gh.prs["bic/aether"][127740].State = "MERGED"
	gh.search["repo:bic/aether is:pr is:open author:John-Kelliher"] = []int{116018}
	at := t0.Add(HotEvery)
	r, calls := round(t, gh, r.State, c, ttl, at)
	if calls != 2 || keys(r) != "aether-pr-127740" || r.Track["bic/aether#127740"].State != "MERGED" {
		t.Fatalf("merge seen: %d calls, writes %q, track %+v", calls, keys(r), r.Track["bic/aether#127740"])
	}
	r, _ = round(t, gh, r.State, c, ttl, at.Add(time.Hour-time.Second))
	if _, ok := r.Track["bic/aether#127740"]; !ok || len(r.Forms) != 0 {
		t.Fatalf("1 s before the ttl: track %v, writes %q", r.Track, keys(r))
	}
	r, _ = round(t, gh, r.State, c, ttl, at.Add(time.Hour))
	if keys(r) != "retire:aether-pr-127740" {
		t.Fatalf("at the ttl: writes %q", keys(r))
	}
	if _, ok := r.Track["bic/aether#127740"]; ok || r.State.Entities["aether-pr-127740"] != nil {
		t.Fatal("retired PR still tracked")
	}
}

// An open PR no longer matching any rule is UNTRACKED, kept the TTL, then retired; it is not read deep again.
func TestAnUnmatchedPRIsUntrackedThenRetired(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"author":["John-Kelliher"]}],"ttl":{"closed":"10m"}}`)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	gh.search["repo:bic/aether is:pr is:open author:John-Kelliher"] = []int{116018}
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if got := r.Track["bic/aether#127740"].State; got != "UNTRACKED" || len(r.Forms) != 0 {
		t.Fatalf("untracked: %q, writes %q", got, keys(r))
	}
	r, _ = round(t, gh, r.State, c, ttl, t0.Add(HotEvery+10*time.Minute))
	if keys(r) != "retire:aether-pr-127740" {
		t.Fatalf("retire: %q", keys(r))
	}
}

// Explicit PR numbers are tracked without a search alias, through the marks query. RED if rule.prs is ignored.
func TestExplicitPRsAreTracked(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, `{"repos":["bic/aether"],"rules":[{"repo":"bic/aether","prs":[131611],"tier":"cold"}]}`)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 2 || keys(r) != "aether-pr-131611" || r.Track["bic/aether#131611"].Tier != "cold" {
		t.Fatalf("explicit: %d calls, writes %q, track %+v", calls, keys(r), r.Track)
	}
	if strings.Contains(gh.calls[0], "search(") {
		t.Fatalf("an explicit-only config searched: %s", gh.calls[0])
	}
	if _, calls := round(t, gh, r.State, c, ttl, t0.Add(ColdEvery-time.Second)); calls != 0 {
		t.Fatalf("cold, 1 s early: %d calls", calls)
	}
}

// Mergeability GitHub has not computed (UNKNOWN after a push) is read again at the next due marks read, though no
// mark moved. RED if Unsettled goes (the form keeps UNKNOWN until something else moves).
func TestUnknownMergeabilityIsReadAgain(t *testing.T) {
	gh := world(t)
	gh.prs["bic/aether"][116018].Mergeable, gh.prs["bic/aether"][116018].MergeSt = "UNKNOWN", "UNKNOWN"
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	gh.prs["bic/aether"][116018].Mergeable, gh.prs["bic/aether"][116018].MergeSt = "MERGEABLE", "BLOCKED"
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if calls != 2 || keys(r) != "aether-pr-116018" {
		t.Fatalf("unsettled: %d calls, writes %q", calls, keys(r))
	}
	if _, calls := round(t, gh, r.State, c, ttl, t0.Add(2*HotEvery)); calls != 1 {
		t.Fatalf("settled: %d calls, want 1 (marks only)", calls)
	}
}

// A failed marks read fails the round with its error and writes nothing; a failed deep read is logged and the PR is
// read again next round (its mark did not advance).
func TestFailedReads(t *testing.T) {
	gh := world(t)
	c, ttl := cfg(t, hotAuthor)
	gh.failGQL = func(q string) error {
		if strings.Contains(q, "search(") {
			return fmt.Errorf("graphql: API rate limit exceeded")
		}
		return nil
	}
	_, err := Run(context.Background(), gh, State{}, Pass{Config: c, ClosedTTL: ttl, Now: t0})
	if err == nil || err.Error() != "marks read: graphql: API rate limit exceeded" {
		t.Fatalf("marks failure: %v", err)
	}
	gh.failGQL = func(q string) error {
		if strings.Contains(q, "...D") {
			return fmt.Errorf("HTTP 502")
		}
		return nil
	}
	r, _ := round(t, gh, State{}, c, ttl, t0)
	if len(r.Forms) != 0 || !strings.Contains(strings.Join(r.Log, "\n"), "deep read of 2 PR(s): HTTP 502 (read again next round)") {
		t.Fatalf("deep failure: writes %q log %q", keys(r), r.Log)
	}
	gh.failGQL = nil
	r, calls := round(t, gh, r.State, c, ttl, t0.Add(HotEvery))
	if calls != 2 || keys(r) != "aether-pr-116018,aether-pr-127740" {
		t.Fatalf("retry: %d calls, writes %q", calls, keys(r))
	}
}

// More PRs than DeepBatch (10) are read in batches of 10: 11 moved PRs are two deep calls. RED if DeepBatch changes.
func TestDeepReadsBatchTen(t *testing.T) {
	if DeepBatch != 10 {
		t.Fatalf("DeepBatch %d", DeepBatch)
	}
	gh := world(t)
	base := gh.prs["bic/aether"][127740]
	var ns []int
	for i := 1; i <= 11; i++ {
		pr := *base
		pr.Number = 200000 + i
		gh.prs["bic/aether"][pr.Number] = &pr
		ns = append(ns, pr.Number)
	}
	gh.search["repo:bic/aether is:pr is:open author:John-Kelliher"] = ns
	c, ttl := cfg(t, hotAuthor)
	r, calls := round(t, gh, State{}, c, ttl, t0)
	if calls != 3 || len(r.Forms) != 11 {
		t.Fatalf("11 PRs: %d calls (want 1 marks + 2 deep), %d writes", calls, len(r.Forms))
	}
}

// An entity form stays within MaxForm (16 KB): a PR whose threads alone exceed it has them trimmed, named in
// sync.trimmed. RED if the trim goes (the write is over 16 KB) or MaxForm changes.
func TestFormSizeCap(t *testing.T) {
	if MaxForm != 16384 {
		t.Fatalf("MaxForm %d", MaxForm)
	}
	gh := world(t)
	pr := gh.prs["bic/aether"][127740]
	big := *pr
	big.Threads.Nodes = nil
	for i := 0; i < 200; i++ {
		big.Threads.Nodes = append(big.Threads.Nodes, pr.Threads.Nodes[0])
		big.Threads.Nodes[i].ID = fmt.Sprintf("PRRT_%0120d", i)
	}
	big.Threads.TotalCount = 200
	gh.prs["bic/aether"][127740] = &big
	c, ttl := cfg(t, hotAuthor)
	r, _ := round(t, gh, State{}, c, ttl, t0)
	for _, f := range r.Forms {
		n := len(f.Set["gh"]) + len(f.Set["parsed"]) + len(f.Set["sync"])
		if n > MaxForm {
			t.Errorf("%s: %d bytes", f.Key, n)
		}
		if f.Key == "aether-pr-127740" && !strings.Contains(string(f.Set["sync"]), `"trimmed":["threads"]`) {
			t.Errorf("trim not named: %s", f.Set["sync"])
		}
	}
}

// Config: one rejecting row per check, and the accepting shape. RED if a check goes.
func TestParseConfig(t *testing.T) {
	if _, ttl, err := ParseConfig(json.RawMessage(hotAuthor)); err != nil || ttl != 24*time.Hour {
		t.Fatalf("valid: %v %v", ttl, err)
	}
	for _, c := range []struct{ desc, raw, want string }{
		{"absent", ``, "config: absent (want {repos, rules})"},
		{"null", `null`, "config: absent (want {repos, rules})"},
		{"unknown field", `{"repos":["a/b"],"rules":[{"repo":"a/b","prs":[1]}],"x":1}`, `config: json: unknown field "x"`},
		{"no repos", `{"repos":[],"rules":[{"repo":"a/b","prs":[1]}]}`, "config: no repos"},
		{"repo not owner/name", `{"repos":["aether"],"rules":[{"repo":"a/b","prs":[1]}]}`, `config: repo "aether" is not owner/name`},
		{"repo injection", `{"repos":["a/b\" x"],"rules":[{"repo":"a/b","prs":[1]}]}`, `config: repo "a/b\" x" is not owner/name`},
		{"same name part", `{"repos":["a/x","b/X"],"rules":[{"repo":"a/b","prs":[1]}]}`, `config: repos a/x and b/X share the form name part "x"`},
		{"no rules", `{"repos":["a/b"],"rules":[]}`, "config: 0 rules (want 1-10)"},
		{"11 rules", `{"repos":["a/b"],"rules":[` + strings.Repeat(`{"repo":"a/b","prs":[1]},`, 10) + `{"repo":"a/b","prs":[1]}]}`, "config: 11 rules (want 1-10)"},
		{"empty rule", `{"repos":["a/b"],"rules":[{"tier":"hot"}]}`, "config: rule 0 selects nothing"},
		{"bad tier", `{"repos":["a/b"],"rules":[{"repo":"a/b","prs":[1],"tier":"lukewarm"}]}`, `config: rule 0: tier "lukewarm" (want hot, warm or cold)`},
		{"bad author", `{"repos":["a/b"],"rules":[{"author":["a b"]}]}`, `config: rule 0: author "a b" is not a login`},
		{"quote in label", `{"repos":["a/b"],"rules":[{"label":["x\"y"]}]}`, `config: rule 0: term "x\"y" (1-100 bytes, no quote, backslash or newline)`},
		{"101-byte tag", `{"repos":["a/b"],"rules":[{"title_tag":["` + strings.Repeat("t", 101) + `"]}]}`, `config: rule 0: term "` + strings.Repeat("t", 101) + `" (1-100 bytes, no quote, backslash or newline)`},
		{"pr 0", `{"repos":["a/b"],"rules":[{"repo":"a/b","prs":[0]}]}`, "config: rule 0: pr 0"},
		{"rule repo not in repos", `{"repos":["a/b"],"rules":[{"repo":"a/c","prs":[1]}]}`, `config: rule 0: repo "a/c" is not in repos`},
		{"prs without a repo", `{"repos":["a/b"],"rules":[{"prs":[1]}]}`, "config: rule 0: prs need a repo"},
		{"a repo giving no child key", `{"repos":["bic/.github"],"rules":[{"author":["a"]}]}`, `config: repo bic/.github gives child key "-github-pr-9999999999", not [a-z0-9][a-z0-9-], 1-100`},
		{"max over 90", `{"repos":["a/b"],"rules":[{"author":["a"]}],"max":91}`, "config: max 91 (want 1-90)"},
		{"max negative", `{"repos":["a/b"],"rules":[{"author":["a"]}],"max":-1}`, "config: max -1 (want 1-90)"},
		{"bad ttl", `{"repos":["a/b"],"rules":[{"repo":"a/b","prs":[1]}],"ttl":{"closed":"-1h"}}`, `config: ttl.closed "-1h" is not a positive duration`},
	} {
		_, _, err := ParseConfig(json.RawMessage(c.raw))
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.desc, err, c.want)
		}
	}
	if c, _, _ := ParseConfig(json.RawMessage(hotAuthor)); c.Max != 80 {
		t.Fatalf("default max %d, want 80", c.Max)
	}
	if c, _, err := ParseConfig(json.RawMessage(`{"repos":["a/b"],"rules":[{"author":["a"]}],"max":90}`)); err != nil || c.Max != 90 {
		t.Fatalf("max 90: %v", err)
	}
	// The cap and a 100-byte term are accepted.
	ok := `{"repos":["a/b"],"rules":[` + strings.Repeat(`{"repo":"a/b","prs":[1]},`, 9) + `{"title_tag":["` + strings.Repeat("t", 100) + `"]}]}`
	if _, _, err := ParseConfig(json.RawMessage(ok)); err != nil {
		t.Fatalf("10 rules, 100-byte tag: %v", err)
	}
}

// The search aliases: one per author (search ANDs author:), one OR-ed per label set and per title-tag set, quoted
// for GraphQL. RED if two authors share one alias (GitHub would answer neither's PRs alone).
func TestSearches(t *testing.T) {
	c, _ := cfg(t, `{"repos":["bic/aether"],"rules":[{"author":["a","b"],"label":["x y","z"],"title_tag":["[Effort"]}]}`)
	var got []string
	for _, s := range Searches(c) {
		got = append(got, s.Alias+"="+s.Query)
	}
	want := `s0=repo:bic/aether is:pr is:open author:a|s1=repo:bic/aether is:pr is:open author:b|` +
		`s2=repo:bic/aether is:pr is:open label:\"x y\",\"z\"|s3=repo:bic/aether is:pr is:open in:title \"[Effort\"`
	if strings.Join(got, "|") != want {
		t.Fatalf("searches:\n got  %s\n want %s", strings.Join(got, "|"), want)
	}
}
