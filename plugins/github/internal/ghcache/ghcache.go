package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/gluck/ebac/sdk/plugin"
)

// GitHub is what a round reads: aliased GraphQL (points) and pull heads over
// ls-refs (0 quota). forge.GH is the real one.
type GitHub interface {
	GraphQL(ctx context.Context, query string) (json.RawMessage, error)
	PullHeads(ctx context.Context, repo string, prs []int) (map[int]string, error)
}

// MaxForm caps an entity form's gh + parsed + sync (DESIGN 10: 16 KB).
const MaxForm = 16 * 1024

// MaxErrors caps sync.errors.
const MaxErrors = 10

// State is the plugin's runtime state (runtime.gh_cache), opaque to the core.
type State struct {
	Entities map[string]*Entity `json:"entities,omitempty"` // by child key
	SearchAt time.Time          `json:"search_at,omitzero"` // the last marks read with searches
}

// Entity is one tracked PR's bookkeeping.
type Entity struct {
	Repo    string    `json:"repo"`
	N       int       `json:"n"`
	Rule    int       `json:"rule"`
	Tier    string    `json:"tier"`
	Since   time.Time `json:"since"`
	Seen    time.Time `json:"seen"`              // last round a rule matched it
	Mark    Mark      `json:"mark"`              // as of the last deep read
	MarkAt  time.Time `json:"mark_at,omitzero"`  // the last marks read of it
	DeepAt  time.Time `json:"deep_at,omitzero"`  // the last deep read
	Digest  string    `json:"digest,omitempty"`  // hash of the gh + parsed last written
	EndedAt time.Time `json:"ended_at,omitzero"` // first seen closed, merged or untracked
	// Unsettled: GitHub had not computed mergeability at the last deep
	// read (UNKNOWN right after a push); the next due marks read re-reads it.
	Unsettled bool `json:"unsettled,omitempty"`
}

// Pass is one round's input.
type Pass struct {
	Config    *Config
	ClosedTTL time.Duration
	Now       time.Time
}

// Result is what a round produces.
type Result struct {
	State  State
	Forms  []plugin.FormWrite
	Track  map[string]Track
	Log    []string
	Points int
}

// Track is one line of the register's index (role form key track).
type Track struct {
	Form  string `json:"form"` // the child key; the core names the form ebac-<critic>-<key>
	Rule  int    `json:"rule"`
	Tier  string `json:"tier"`
	Since string `json:"since"`
	State string `json:"state"`
}

// SyncKey is the entity form's sync key.
type SyncKey struct {
	Version string   `json:"version"` // head|updated_at|state|rollup at the read
	At      string   `json:"at"`
	Errors  []string `json:"errors,omitempty"`
	Trimmed []string `json:"trimmed,omitempty"` // fields cut to fit MaxForm
}

func (e *Entity) ended() bool { return e.Mark.State == "MERGED" || e.Mark.State == "CLOSED" }

// Run is one round.
func Run(ctx context.Context, gh GitHub, prev State, p Pass) (Result, error) {
	r := Result{State: State{Entities: map[string]*Entity{}, SearchAt: prev.SearchAt}}
	for k, e := range prev.Entities {
		c := *e
		r.State.Entities[k] = &c
	}
	ents := r.State.Entities
	now := p.Now.UTC()
	logf := func(f string, a ...any) { r.Log = append(r.Log, fmt.Sprintf(f, a...)) }

	// 1. Pull heads over ls-refs, every round, 0 quota: a moved head is read
	// in full now, whatever its tier.
	moved := map[string]bool{}
	var open []Ref
	for _, e := range ents {
		if !e.ended() {
			open = append(open, Ref{e.Repo, e.N})
		}
	}
	repos, byR := byRepo(open)
	for _, repo := range repos {
		heads, err := gh.PullHeads(ctx, repo, byR[repo])
		if err != nil {
			logf("ls-refs %s: %v (marks decide this round)", repo, err)
			continue
		}
		for _, n := range byR[repo] {
			e := ents[ChildKey(repo, n)]
			if h, ok := heads[n]; ok && h != e.Mark.Head {
				moved[ChildKey(repo, n)] = true
			}
		}
	}

	// 2. One marks read: the searches when the fastest rule is due, and the
	// due tracked PRs the searches do not cover (warm, cold, explicit,
	// ended or no longer matching).
	fastest := ColdEvery
	for _, rl := range p.Config.Rules {
		if d, _ := Every(rl.Tier); d < fastest {
			fastest = d
		}
	}
	searches := Searches(p.Config)
	doSearch := len(searches) > 0 && !now.Before(r.State.SearchAt.Add(fastest))
	inSearch := map[string]bool{}
	matched := map[string]int{} // the first rule that matched each PR this round
	var due []Ref
	explicit := map[string]int{}
	for i, rl := range p.Config.Rules {
		for _, repo := range p.Config.Repos {
			for _, n := range rl.PRs {
				if _, ok := explicit[ChildKey(repo, n)]; !ok {
					explicit[ChildKey(repo, n)] = i
				}
			}
		}
	}
	for k, rule := range explicit {
		if _, ok := ents[k]; !ok {
			repo, n := refOfKey(p.Config, k)
			ents[k] = &Entity{Repo: repo, N: n, Rule: rule, Tier: tierOr(p.Config.Rules[rule].Tier), Since: now}
		}
	}
	for k, e := range ents {
		every, _ := Every(e.Tier)
		if e.ended() || !e.EndedAt.IsZero() {
			every = ColdEvery // closed, merged or no longer matching: only its end is news
		}
		if !moved[k] && (e.MarkAt.IsZero() || !now.Before(e.MarkAt.Add(every))) {
			due = append(due, Ref{e.Repo, e.N})
		}
	}
	marks := map[string]Mark{}
	if doSearch || len(due) > 0 {
		var ss []Search
		if doSearch {
			ss = searches
		}
		data, err := gh.GraphQL(ctx, MarksQuery(ss, due))
		if err != nil {
			return r, fmt.Errorf("marks read: %w", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return r, fmt.Errorf("marks read: decode: %w", err)
		}
		for _, s := range ss {
			var sr struct {
				IssueCount int        `json:"issueCount"`
				Nodes      []MarkNode `json:"nodes"`
			}
			if err := json.Unmarshal(raw[s.Alias], &sr); err != nil {
				return r, fmt.Errorf("marks read: %s: %w", s.Alias, err)
			}
			if sr.IssueCount > SearchFirst {
				logf("search %s (%s): %d PRs, only the first %d read", s.Alias, s.Query, sr.IssueCount, SearchFirst)
			}
			for _, n := range sr.Nodes {
				if n.Number == 0 {
					continue
				}
				k := ChildKey(s.Repo, n.Number)
				inSearch[k] = true
				marks[k] = n.Mark()
				if e := ents[k]; e == nil {
					ents[k] = &Entity{Repo: s.Repo, N: n.Number, Rule: s.Rule, Tier: tierOr(p.Config.Rules[s.Rule].Tier), Since: now}
				}
				if r0, ok := matched[k]; !ok || s.Rule < r0 {
					matched[k] = s.Rule
				}
			}
		}
		if doSearch {
			r.State.SearchAt = now
		}
		aliases := repoAliases(due)
		for alias, repo := range aliases {
			var prs map[string]*MarkNode
			if err := json.Unmarshal(raw[alias], &prs); err != nil {
				return r, fmt.Errorf("marks read: %s: %w", alias, err)
			}
			for _, n := range prs {
				if n == nil {
					continue
				}
				marks[ChildKey(repo, n.Number)] = n.Mark()
			}
		}
		for _, ref := range due {
			k := ChildKey(ref.Repo, ref.N)
			if _, ok := marks[k]; !ok {
				logf("%s: no marks answer (deleted or unreadable)", k)
			}
		}
		for k := range marks {
			if e := ents[k]; e != nil {
				e.MarkAt = now
			}
		}
	}
	for k, rule := range explicit {
		if r0, ok := matched[k]; !ok || rule < r0 {
			matched[k] = rule
		}
	}
	for k, rule := range matched {
		e := ents[k]
		e.Seen, e.Rule, e.Tier = now, rule, tierOr(p.Config.Rules[rule].Tier)
	}

	// 3. Deep reads: new entities, moved marks, moved heads; ended ones
	// once (their last state), settled mergeability re-read.
	var deep []Ref
	for k, e := range ents {
		m, ok := marks[k]
		switch {
		case moved[k]:
		case ok && (e.DeepAt.IsZero() || m != e.Mark || e.Unsettled):
		default:
			continue
		}
		deep = append(deep, Ref{e.Repo, e.N})
	}
	sort.Slice(deep, func(i, j int) bool {
		if deep[i].Repo != deep[j].Repo {
			return deep[i].Repo < deep[j].Repo
		}
		return deep[i].N < deep[j].N
	})
	for i := 0; i < len(deep); i += DeepBatch {
		part := deep[i:min(i+DeepBatch, len(deep))]
		data, err := gh.GraphQL(ctx, DeepQuery(part))
		if err != nil {
			logf("deep read of %d PR(s): %v (retried next round)", len(part), err)
			continue
		}
		var raw map[string]map[string]*PR
		if err := json.Unmarshal(data, &raw); err != nil {
			logf("deep read decode: %v (retried next round)", err)
			continue
		}
		for alias, repo := range repoAliases(part) {
			for _, pr := range raw[alias] {
				if pr == nil {
					continue
				}
				k := ChildKey(repo, pr.Number)
				e := ents[k]
				if e == nil {
					continue
				}
				e.Mark, e.DeepAt, e.MarkAt = pr.Mark(), now, now
				e.Unsettled = pr.State == "OPEN" && (pr.Mergeable == "UNKNOWN" || pr.MergeSt == "UNKNOWN")
				fw, digest := entityWrite(k, pr, now)
				if digest != e.Digest {
					r.Forms = append(r.Forms, fw)
					e.Digest = digest
				}
			}
		}
	}

	// 4. Lifetimes: an ended or no longer matching PR keeps its form for
	// ClosedTTL, then retires (its form is renamed and its lease lapses).
	for k, e := range ents {
		_, isExplicit := explicit[k]
		gone := e.ended() || (doSearch && !inSearch[k] && !isExplicit)
		switch {
		case !gone:
			e.EndedAt = time.Time{}
		case e.EndedAt.IsZero():
			e.EndedAt = now
		case !now.Before(e.EndedAt.Add(p.ClosedTTL)):
			if !e.DeepAt.IsZero() {
				r.Forms = append(r.Forms, plugin.FormWrite{Key: k, Retire: true})
			}
			delete(ents, k)
		}
	}
	r.Track = map[string]Track{}
	for k, e := range ents {
		st := e.Mark.State
		if !e.EndedAt.IsZero() && !e.ended() {
			st = "UNTRACKED"
		}
		r.Track[fmt.Sprintf("%s#%d", e.Repo, e.N)] = Track{Form: k, Rule: e.Rule, Tier: e.Tier, Since: e.Since.Format(time.RFC3339), State: st}
	}
	sort.Slice(r.Forms, func(i, j int) bool { return r.Forms[i].Key < r.Forms[j].Key })
	logf("%d tracked, %d head move(s), %d due, search %v, %d deep, %d form write(s)", len(ents), len(moved), len(due), doSearch, len(deep), len(r.Forms))
	return r, nil
}

func tierOr(t string) string {
	if t == "" {
		return "hot"
	}
	return t
}

func refOfKey(c *Config, key string) (string, int) {
	for _, repo := range c.Repos {
		var n int
		if _, err := fmt.Sscanf(key, repoKey(repo)+"-pr-%d", &n); err == nil && ChildKey(repo, n) == key {
			return repo, n
		}
	}
	return "", 0
}

// entityWrite builds one entity form write and the digest of its gh+parsed.
func entityWrite(key string, pr *PR, now time.Time) (plugin.FormWrite, string) {
	g := GHOf(pr)
	ps := Parse(pr)
	sy := SyncKey{Version: pr.Mark().Head + "|" + pr.UpdatedAt + "|" + pr.State + "|" + pr.Mark().Rollup, At: now.Format(time.RFC3339)}
	sy.Errors = ps.Errors
	if pr.Threads.TotalCount > len(pr.Threads.Nodes) {
		sy.Errors = append(sy.Errors, fmt.Sprintf("threads: %d of %d read", len(pr.Threads.Nodes), pr.Threads.TotalCount))
	}
	if len(sy.Errors) > MaxErrors {
		sy.Errors = sy.Errors[:MaxErrors]
	}
	size := func() int { return len(mustJSON(g)) + len(mustJSON(ps)) + len(mustJSON(sy)) }
	for _, trim := range []struct {
		name string
		do   func()
	}{
		{"threads", func() { g.Threads = nil }},
		{"body.sections", func() { g.Body.Sections = nil }},
		{"reviews", func() { g.Reviews = nil }},
		{"checks.jobs_failed", func() {
			for _, c := range g.Checks {
				c.JobsFailed = nil
			}
		}},
	} {
		if size() <= MaxForm {
			break
		}
		trim.do()
		sy.Trimmed = append(sy.Trimmed, trim.name)
	}
	gj, pj := mustJSON(g), mustJSON(ps)
	return plugin.FormWrite{Key: key, Set: map[string]json.RawMessage{"gh": gj, "parsed": pj, "sync": mustJSON(sy)}},
		hash12(string(gj) + string(pj))
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
