package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// GitHub is what a round reads: aliased GraphQL answered alias by alias
// (points; a missing PR is a NOT_FOUND on its path, never a failed call) and
// pull heads over ls-refs (0 quota). forge.GH is the real one.
type GitHub interface {
	GraphQLPartial(ctx context.Context, query string) (json.RawMessage, []forge.GQLError, error)
	PullHeads(ctx context.Context, repo string, prs []int) (map[int]string, error)
	// Mutate is one GraphQL write (slice 3: sync.go).
	Mutate(ctx context.Context, mutation string, vars map[string]string) (json.RawMessage, error)
}

// MaxForm caps an entity form's gh + parsed + sync (DESIGN 10: 16 KB).
const MaxForm = 16 * 1024

// MaxErrors caps sync.errors.
const MaxErrors = 10

// State is the plugin's runtime state (runtime.gh_cache), opaque to the core.
type State struct {
	Entities map[string]*Entity `json:"entities,omitempty"` // by child key
	SearchAt time.Time          `json:"search_at,omitzero"` // the last marks read with searches
	// Wrote are the child keys of the last reply's form writes, in order: the
	// core's runtime.forms_landed says how many landed; the rest are redone.
	Wrote []string `json:"wrote,omitempty"`
}

// Entity is one tracked PR's or issue's bookkeeping.
type Entity struct {
	Kind    string    `json:"kind,omitempty"` // KindIssue, or "" for a PR
	Repo    string    `json:"repo"`
	N       int       `json:"n"`
	Rule    int       `json:"rule"`
	Tier    string    `json:"tier"`
	Since   time.Time `json:"since"`
	Seen    time.Time `json:"seen"`              // last round a rule matched it
	State   string    `json:"state,omitempty"`   // OPEN | CLOSED | MERGED, from the newest marks or deep read
	Mark    Mark      `json:"mark"`              // as of the last deep read
	MarkAt  time.Time `json:"mark_at,omitzero"`  // the last marks read of it
	DeepAt  time.Time `json:"deep_at,omitzero"`  // the last deep read whose form was written
	Digest  string    `json:"digest,omitempty"`  // hash of the gh + parsed last written
	EndedAt time.Time `json:"ended_at,omitzero"` // first seen closed, merged or untracked
	// Unsettled: GitHub answered UNKNOWN mergeability at the last deep read.
	// The marks carry mergeability, so its settling moves the mark; a PR
	// GitHub keeps at UNKNOWN is still read deep every UnknownEvery, so a
	// stuck answer cannot freeze its form.
	Unsettled bool `json:"unsettled,omitempty"`
	// Redo: read and write it again this round (its last write did not land,
	// or its deep read failed).
	Redo bool `json:"redo,omitempty"`

	// The write path (sync.go). Base and RO are what the form showed when
	// the sync last agreed with GitHub (Pend until its write lands); Alarms
	// and Conflicts are mirrored to sync.alarm / sync.conflicts until fixed
	// or acked; Tries counts a push's stale or failed rounds; Sent are the
	// hashes of replies posted (never posted twice).
	Base      *Base                      `json:"base,omitempty"`
	RO        string                     `json:"ro,omitempty"`
	Pend      *Agreed                    `json:"pend,omitempty"`
	pushing   *Pushed                    // this round's own GitHub writes (sync.go), into Pend
	Alarms    map[string]Alarm           `json:"alarms,omitempty"`
	Conflicts map[string]json.RawMessage `json:"conflicts,omitempty"`
	Tries     map[string]int             `json:"tries,omitempty"`
	Sent      []string                   `json:"sent,omitempty"`
	// OursThreads are, per review thread longer than ThreadLast, our comments
	// in it as of a full read at its count N (#45): read again only when N moves.
	OursThreads map[string]OursThread `json:"ours_threads,omitempty"`
}

// OursThread is our comments in one long review thread, read whole at N.
type OursThread struct {
	N        int                      `json:"n"`
	Comments map[string]CachedComment `json:"comments,omitempty"`
	Partial  bool                     `json:"partial,omitempty"` // more than the first 100 comments: ours past them unread
}

// MaxFullThreads bounds the long threads read whole in one round, over every
// deep batch (one point each at most; the rest, and their entities, are read
// again next round).
const MaxFullThreads = 20

// Pass is one round's input.
type Pass struct {
	Config    *Config
	ClosedTTL time.Duration
	Now       time.Time
	// Children are the child forms the core holds (runtime.children: key ->
	// @id) and Landed how many of the last reply's form writes landed
	// (runtime.forms_landed).
	Children map[string]string
	Landed   int
	// Owned are the entity forms' gh and sync as the core read them this
	// round (Request.Owned); nil: the core passed none (no write path).
	Owned map[string]map[string]json.RawMessage
	// Auth says why a write may not go out ("" if it may): the trusted
	// scope (model.AuthorizeGHWrite). nil refuses every write.
	Auth func(e model.GHEntity, field string) string
	// Maintainer is the trusted role the alarms wake; Critic names the
	// forms in the pings.
	Maintainer string
	Critic     string
	// Ours is the trusted account login whose texts are ours (lower case
	// compare): their bodies are trimmed last.
	Ours string
}

// Result is what a round produces.
type Result struct {
	State State
	Forms []plugin.FormWrite
	Track map[string]Track
	Pings []plugin.Ping
	Log   []string
	// Telemetry is the round's record (telemetry.go).
	Telemetry Telemetry
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
	// Alarm is sync.alarm.<kind>.<field> (DESIGN 8) and Conflicts the
	// form's values GitHub's overrode (sync.conflicts.<field>).
	Alarm     map[string]map[string]Alarm `json:"alarm,omitempty"`
	Conflicts map[string]json.RawMessage  `json:"conflicts,omitempty"`
}

func (e *Entity) ended() bool { return e.State == "MERGED" || e.State == "CLOSED" }

// hasForm: the entity's child form exists or a write of it is in flight.
func hasForm(p Pass, k string, e *Entity) bool { _, ok := p.Children[k]; return ok || e.Digest != "" }

// aliasErrs splits a partial answer's errors: NOT_FOUND on a PR alias
// (path [rN, pN]) names a missing PR; an error under a PR alias is that PR's;
// anything else fails the call.
func aliasErrs(errs []forge.GQLError, aliases map[string]string) (missing map[string]bool, per map[string][]string, other error) {
	missing, per = map[string]bool{}, map[string][]string{}
	for _, e := range errs {
		if len(e.Path) >= 2 && (strings.HasPrefix(e.Path[1], "p") || strings.HasPrefix(e.Path[1], "i")) {
			if repo, ok := aliases[e.Path[0]]; ok {
				var n int
				if _, err := fmt.Sscanf(e.Path[1][1:], "%d", &n); err == nil {
					k := ChildKey(repo, n)
					if e.Path[1][0] == 'i' {
						k = IssueKey(repo, n)
					}
					if len(e.Path) == 2 && e.Type == "NOT_FOUND" {
						missing[k] = true
					} else {
						per[k] = append(per[k], cut(fmt.Sprintf("%s: %s %s", strings.Join(e.Path[2:], "."), e.Type, e.Message), 200))
					}
					continue
				}
			}
		}
		if other == nil {
			other = fmt.Errorf("graphql: %s %s at %s", e.Type, e.Message, strings.Join(e.Path, "."))
		}
	}
	return missing, per, other
}

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
	retire := map[string]bool{}

	// 0. Landing: the last reply's writes past forms_landed did not land;
	// an entity written but not among the core's children was never minted.
	// Both are read and written again. A child the core holds that the
	// cache no longer tracks (a retire that did not land) is retired again.
	// why is each entity's reason to be read deep this round (telemetry):
	// the first reason set wins.
	why := map[string]string{}
	for k, e := range ents {
		if e.Redo {
			why[k] = "retry" // the last round's deep read failed or its write did not land
		}
	}
	redoBecause := func(k string, e *Entity, reason string) {
		e.Redo = true
		if why[k] == "" || why[k] == "retry" {
			why[k] = reason
		}
	}
	unlanded := map[string]bool{}
	if p.Landed < len(prev.Wrote) {
		logf("%d of %d form write(s) of the last round did not land: redone", len(prev.Wrote)-p.Landed, len(prev.Wrote))
		for _, k := range prev.Wrote[p.Landed:] {
			unlanded[k] = true
			if e := ents[k]; e != nil {
				redoBecause(k, e, "unlanded")
				e.Digest = "" // what was sent never landed: the rewrite must not be skipped as unchanged
			}
		}
	}
	// A base agreed last round counts once the form shows it: its write
	// landed (or was not needed). An unlanded one is dropped: the form still
	// shows the old base, and a diff against the new one would read GitHub's
	// pulled change as a seat's edit and push it back.
	for k, e := range ents {
		switch {
		case e.Pend != nil && !unlanded[k]:
			b := e.Pend.Base
			e.Base, e.RO = &b, e.Pend.RO
		case e.Pend != nil && e.Pend.Pushed != nil && e.Base != nil:
			// The form write did not land, but our GitHub writes did: GitHub
			// and the base agree on those fields (a form still showing the
			// old value asks to write it back).
			b := e.Pend.Pushed.onto(*e.Base)
			e.Base = &b
		}
		e.Pend = nil
	}
	for k, e := range ents {
		if _, ok := p.Children[k]; !ok && e.Digest != "" && !e.Redo {
			redoBecause(k, e, "unminted")
		}
	}
	for k := range p.Children {
		if ents[k] == nil {
			retire[k] = true
		}
	}

	// 1. Pull heads over ls-refs, every round, 0 quota: a moved head is read
	// in full now, whatever its tier.
	moved := map[string]bool{}
	var open []Ref
	for _, e := range ents {
		if !e.ended() && e.Kind != KindIssue { // ls-refs knows pull heads only
			open = append(open, refOf(e))
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
	// due tracked PRs the searches do not answer (warm, cold, explicit,
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
	explicit := map[string]int{}
	for i, rl := range p.Config.Rules {
		for _, n := range rl.PRs {
			k := ChildKey(rl.Repo, n)
			if _, ok := explicit[k]; !ok {
				explicit[k] = i
			}
			if ents[k] == nil {
				ents[k] = &Entity{Repo: rl.Repo, N: n, Rule: i, Tier: tierOr(rl.Tier), Since: now}
			}
		}
		for _, n := range rl.Issues {
			k := IssueKey(rl.Repo, n)
			if _, ok := explicit[k]; !ok {
				explicit[k] = i
			}
			if ents[k] == nil {
				ents[k] = &Entity{Kind: KindIssue, Repo: rl.Repo, N: n, Rule: i, Tier: tierOr(rl.Tier), Since: now}
			}
		}
	}
	var due []Ref
	for k, e := range ents {
		every, _ := Every(e.Tier)
		if e.ended() || !e.EndedAt.IsZero() {
			every = ColdEvery // closed, merged or no longer matching: only its end is news
		}
		// A searched PR is asked by its alias too (the same call): one that
		// left the search shows why (merged, closed) in this very round.
		if !moved[k] && (e.MarkAt.IsZero() || !now.Before(e.MarkAt.Add(every))) {
			due = append(due, refOf(e))
		}
	}
	marks := map[string]Mark{}
	missing := map[string]bool{}
	complete := true // every search answered whole (no PR past SearchFirst)
	if doSearch || len(due) > 0 {
		var ss []Search
		if doSearch {
			ss = searches
		}
		aliases := repoAliases(due)
		data, errs, err := gh.GraphQLPartial(ctx, MarksQuery(ss, due))
		if err != nil {
			return r, fmt.Errorf("marks read: %w", err)
		}
		miss, _, other := aliasErrs(errs, aliases)
		if other != nil {
			return r, fmt.Errorf("marks read: %w", other)
		}
		missing = miss
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
				complete = false
				logf("search %s (%s): %d PRs, only the first %d read; nothing untracked this round", s.Alias, s.Query, sr.IssueCount, SearchFirst)
			}
			for _, n := range sr.Nodes {
				if n.Number == 0 {
					continue
				}
				k := ChildKey(s.Repo, n.Number)
				inSearch[k] = true
				marks[k] = n.Mark()
				if ents[k] == nil {
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
		for alias, repo := range aliases {
			var nodes map[string]json.RawMessage
			if err := json.Unmarshal(raw[alias], &nodes); err != nil {
				return r, fmt.Errorf("marks read: %s: %w", alias, err)
			}
			for name, node := range nodes {
				if len(node) == 0 || string(node) == "null" {
					continue
				}
				if strings.HasPrefix(name, "i") {
					var n IssueMarkNode
					if err := json.Unmarshal(node, &n); err != nil {
						return r, fmt.Errorf("marks read: %s.%s: %w", alias, name, err)
					}
					marks[IssueKey(repo, n.Number)] = n.Mark()
					continue
				}
				var n MarkNode
				if err := json.Unmarshal(node, &n); err != nil {
					return r, fmt.Errorf("marks read: %s.%s: %w", alias, name, err)
				}
				marks[ChildKey(repo, n.Number)] = n.Mark()
			}
		}
		for k, m := range marks {
			if e := ents[k]; e != nil {
				e.MarkAt, e.State = now, m.State
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

	// 3. Lifetimes: a PR that does not resolve (deleted, an issue number) is
	// dropped now; an ended or no longer matching one keeps its form for
	// ClosedTTL, then retires (renamed; its lease lapses).
	for k := range missing {
		what := "a PR"
		if e := ents[k]; e != nil && e.Kind == KindIssue {
			what = "an issue"
		}
		logf("%s: not found (deleted, transferred or not %s): dropped", k, what)
		if e := ents[k]; e != nil && hasForm(p, k, e) {
			retire[k] = true
		}
		delete(ents, k)
	}
	for k, e := range ents {
		_, isExplicit := explicit[k]
		gone := e.ended() || (doSearch && complete && !inSearch[k] && !isExplicit)
		switch {
		case !gone:
			e.EndedAt = time.Time{}
		case e.EndedAt.IsZero():
			e.EndedAt = now
		case !now.Before(e.EndedAt.Add(p.ClosedTTL)):
			if hasForm(p, k, e) {
				retire[k] = true
			}
			delete(ents, k)
		}
	}

	// 4. The cap: at most Config.Max forms. Over it, the longest-ended forms
	// retire first; new PRs past what is left are not admitted (logged).
	var withForm, fresh, ended []string
	for k, e := range ents {
		switch {
		case hasForm(p, k, e):
			withForm = append(withForm, k)
			if !e.EndedAt.IsZero() {
				ended = append(ended, k)
			}
		default:
			fresh = append(fresh, k)
		}
	}
	if over := len(withForm) + len(fresh) - p.Config.Max; over > 0 {
		sort.Slice(ended, func(i, j int) bool {
			a, b := ents[ended[i]], ents[ended[j]]
			if !a.EndedAt.Equal(b.EndedAt) {
				return a.EndedAt.Before(b.EndedAt)
			}
			return ended[i] < ended[j]
		})
		for _, k := range ended[:min(over, len(ended))] {
			retire[k] = true
			delete(ents, k)
			over--
		}
		if over > 0 {
			// Admit the first rules first, then the newest numbers.
			sort.Slice(fresh, func(i, j int) bool {
				a, b := ents[fresh[i]], ents[fresh[j]]
				if a.Rule != b.Rule {
					return a.Rule < b.Rule
				}
				return a.N > b.N
			})
			cutAt := max(len(fresh)-over, 0)
			for _, k := range fresh[cutAt:] {
				delete(ents, k)
			}
			logf("cap %d forms: %d PR(s) not tracked: %s", p.Config.Max, len(fresh)-cutAt, strings.Join(fresh[cutAt:], ","))
		}
	}

	// 4b. The entity forms as the core read them: acked alarms clear; a form
	// asking for a write, edited outside its write fields or unparseable is
	// read deep this round (the three-way runs on a fresh GitHub read).
	sy := &syncer{ctx: ctx, gh: gh, p: p, now: now, logf: logf, children: p.Children}
	forms := map[string]*formView{}
	forced := map[string]bool{}
	for k, e := range ents {
		owned, ok := p.Owned[k]
		if !ok {
			continue
		}
		fv := readForm(owned)
		forms[k] = fv
		for _, key := range fv.acks {
			if _, had := e.Alarms[key]; had {
				delete(e.Alarms, key)
				_, field, _ := strings.Cut(key, ".")
				delete(e.Conflicts, field)
				delete(e.Tries, field)
				redoBecause(k, e, "ack")
				forced[k] = true
				logf("%s: sync.alarm.%s acked: cleared", k, key)
			}
		}
		switch {
		case fv.bad != nil:
			redoBecause(k, e, "unparseable")
		case fv.gh != nil && e.Base == nil:
			// No base yet (a form written before the write path): read it
			// deep now, so a base is agreed within one round of the rollout.
			redoBecause(k, e, "no-base")
		case fv.gh != nil && pending(fv.gh, e.Base):
			redoBecause(k, e, "pending-write")
		case fv.gh != nil && ((e.RO != "" && roHash(fv.gh) != e.RO) || len(fv.unknown) > 0):
			redoBecause(k, e, "form-edit")
		}
	}

	// 5. Deep reads: new entities, moved marks (mergeability and the checks
	// digest included), moved heads and redos (a pending write among them).
	var deep []Ref
	for k, e := range ents {
		m, ok := marks[k]
		stuck := e.Unsettled && !now.Before(e.DeepAt.Add(UnknownEvery))
		if moved[k] || e.Redo || (ok && (e.DeepAt.IsZero() || m != e.Mark || stuck)) {
			deep = append(deep, refOf(e))
			r.Telemetry.Deep = append(r.Telemetry.Deep, deepWhy(k, e, moved[k], why[k], ok, m, stuck))
		}
	}
	sort.Slice(deep, func(i, j int) bool {
		if deep[i].Repo != deep[j].Repo {
			return deep[i].Repo < deep[j].Repo
		}
		return deep[i].N < deep[j].N
	})
	for i := 0; i < len(deep); i += DeepBatch {
		part := deep[i:min(i+DeepBatch, len(deep))]
		aliases := repoAliases(part)
		redo := func(why string) {
			logf("deep read of %d PR(s): %s (read again next round)", len(part), why)
			for _, ref := range part {
				kind := KindPR
				if ref.Issue {
					kind = KindIssue
				}
				if e := ents[keyOf(kind, ref.Repo, ref.N)]; e != nil {
					e.Redo = true
				}
			}
		}
		data, errs, err := gh.GraphQLPartial(ctx, DeepQuery(part))
		if err != nil {
			redo(err.Error())
			continue
		}
		miss, per, other := aliasErrs(errs, aliases)
		if other != nil {
			redo(other.Error())
			continue
		}
		var items []deepItem
		var raw map[string]map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			redo("decode: " + err.Error())
			continue
		}
		for k := range miss {
			logf("%s: not found at the deep read: dropped", k)
			if e := ents[k]; e != nil && hasForm(p, k, e) {
				retire[k] = true
			}
			delete(ents, k)
		}
		for alias, repo := range aliases {
			for name, node := range raw[alias] {
				if len(node) == 0 || string(node) == "null" {
					continue
				}
				// An issue is read as a PR with no PR-only parts (asPR), and
				// its gh carries only what an issue has (GHOfIssue).
				var k string
				var pr *PR
				var g GH
				var mark Mark
				var isIssue bool
				if strings.HasPrefix(name, "i") {
					var is Issue
					if err := json.Unmarshal(node, &is); err != nil {
						logf("%s.%s: decode: %v (read again next round)", alias, name, err)
						continue
					}
					k, pr, g, mark, isIssue = IssueKey(repo, is.Number), is.asPR(), GHOfIssue(&is), is.Mark(), true
				} else {
					pr = &PR{}
					if err := json.Unmarshal(node, pr); err != nil {
						logf("%s.%s: decode: %v (read again next round)", alias, name, err)
						continue
					}
					k, g, mark = ChildKey(repo, pr.Number), GHOf(pr), pr.Mark()
				}
				e := ents[k]
				if e == nil {
					continue
				}
				items = append(items, deepItem{k: k, e: e, pr: pr, g: g, mark: mark, issue: isIssue})
			}
		}
		// Our comments deeper in a long thread than its newest ThreadLast: read
		// whole once per count, so a seat can edit ours wherever it sits.
		if p.Ours != "" {
			sy.oursThreads(items)
		}
		for _, it := range items {
			k, e, pr, g, mark := it.k, it.e, it.pr, it.g, it.mark
			// A long thread left unread whole (the round's MaxFullThreads spent)
			// reads the entity again next round.
			e.Mark, e.DeepAt, e.MarkAt, e.State, e.Redo = mark, now, now, pr.State, it.again
			e.Unsettled = e.Kind != KindIssue && pr.State == "OPEN" && (pr.Mergeable == "UNKNOWN" || pr.MergeSt == "UNKNOWN")
			_, held := p.Children[k]
			fv := forms[k]
			if p.Owned != nil && held && fv == nil {
				// The core could not read the form: writing it blind could
				// clobber a seat's edit. Read and written next round.
				e.Redo = true
				logf("%s: form unreadable this round: not written", k)
				continue
			}
			g, notes := sy.entity(k, e, pr, g, fv)
			fw, digest, final := entityWrite(k, pr, g, append(per[k], notes...), e, now, p.Ours)
			e.Pend.RO = roHash(&final)
			if digest != e.Digest || !held || forced[k] || (fv != nil && !sameJSON(fv.ghRaw, fw.Set["gh"])) {
				if fv != nil {
					// Every key this write overwrites must still be as the
					// core read it, including absent (a seat creating it
					// meanwhile moved the form).
					fw.Expect = map[string]json.RawMessage{}
					if len(fv.ghRaw) > 0 {
						fw.Expect["gh"] = fv.ghRaw
					} else {
						fw.ExpectAbsent = append(fw.ExpectAbsent, "gh")
					}
					if len(fv.syncRaw) > 0 {
						fw.Expect["sync"] = fv.syncRaw
					} else {
						fw.ExpectAbsent = append(fw.ExpectAbsent, "sync")
					}
				}
				r.Forms = append(r.Forms, fw)
				e.Digest = digest
			}
		}
	}
	for k := range retire {
		if _, held := p.Children[k]; held { // never minted: nothing to retire
			r.Forms = append(r.Forms, plugin.FormWrite{Key: k, Retire: true})
		}
	}

	r.Track = map[string]Track{}
	for k, e := range ents {
		st := e.State
		if !e.EndedAt.IsZero() && !e.ended() {
			st = "UNTRACKED"
		}
		r.Track[fmt.Sprintf("%s#%d", e.Repo, e.N)] = Track{Form: k, Rule: e.Rule, Tier: e.Tier, Since: e.Since.Format(time.RFC3339), State: st}
	}
	r.Pings = sy.pings(p.Critic)
	r.Telemetry.finish(now, len(ents), len(due), doSearch, inSearch, due, r.Forms, sy)
	sort.Slice(r.Forms, func(i, j int) bool { return r.Forms[i].Key < r.Forms[j].Key })
	for _, f := range r.Forms {
		r.State.Wrote = append(r.State.Wrote, f.Key)
	}
	logf("%d tracked, %d head move(s), %d due, search %v, %d deep, %d form write(s)", len(ents), len(moved), len(due), doSearch, len(deep), len(r.Forms))
	return r, nil
}

func tierOr(t string) string {
	if t == "" {
		return "hot"
	}
	return t
}

// entityWrite builds one entity form write of g (GHOf(pr) after the sync),
// the digest of its gh, parsed, alarms and conflicts, and the gh as written
// (after trims). errs are the PR's partial-read errors and the sync's notes.
func entityWrite(key string, pr *PR, g GH, errs []string, e *Entity, now time.Time, ours string) (plugin.FormWrite, string, GH) {
	ps := Parse(pr)
	m := pr.Mark()
	sy := SyncKey{Version: m.Head + "|" + pr.UpdatedAt + "|" + pr.State + "|" + m.Rollup, At: now.Format(time.RFC3339),
		Alarm: syncKeyAlarms(e), Conflicts: e.Conflicts}
	if len(sy.Conflicts) == 0 {
		sy.Conflicts = nil
	}
	sy.Errors = append(append([]string{}, errs...), ps.Errors...)
	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"threads", len(pr.Threads.Nodes), pr.Threads.TotalCount},
		{"reviews", len(pr.Reviews.Nodes), pr.Reviews.TotalCount},
		{"comments", len(pr.Comments.Nodes), pr.Comments.TotalCount},
		{"labels", len(pr.Labels.Nodes), pr.Labels.TotalCount},
		{"checks", checkCount(pr), checkTotal(pr)},
	} {
		if c.want > c.got {
			sy.Errors = append(sy.Errors, fmt.Sprintf("truncated: %s: %d of %d read (the newest)", c.what, c.got, c.want))
		}
	}
	if len(sy.Errors) > MaxErrors {
		sy.Errors = sy.Errors[:MaxErrors]
	}
	size := func() int { return len(mustJSON(g.wire())) + len(mustJSON(ps)) + len(mustJSON(sy)) }
	isOurs := func(c CachedComment) bool { return ours != "" && strings.EqualFold(c.Author, ours) }
	fits := func() bool { return size() <= MaxForm }
	// The texts go first (#45), ours last: others' thread comment bodies,
	// others' conversation comment bodies, the body's text, ours; each cut
	// to SectionHead and marked cut (an edit of it is refused).
	cutThreads := func(mine bool) {
		for id, t := range g.Threads {
			t.Comments = cutAll(t.Comments, func(c CachedComment) bool { return isOurs(c) == mine })
			g.Threads[id] = t
		}
	}
	for _, trim := range []struct {
		name string
		do   func()
	}{
		{"threads.comments", func() { cutThreads(false) }},
		{"comments", func() { g.Comments = cutAll(g.Comments, func(c CachedComment) bool { return !isOurs(c) }) }},
		{"body.text", func() {
			if len(g.Body.Text) > SectionHead {
				g.Body.Text, g.Body.Cut = cut(g.Body.Text, SectionHead), true
			}
		}},
		{"comments.ours", func() {
			g.Comments = cutAll(g.Comments, isOurs)
			cutThreads(true)
		}},
		// Many short comments still over the cap: drop them whole, oldest
		// first, others' before ours (comments_total keeps the real count).
		{"comments.dropped", func() { dropOldest(&g.Comments, func(c CachedComment) bool { return !isOurs(c) }, fits) }},
		{"threads.comments.dropped", func() { dropThreadComments(&g, func(c CachedComment) bool { return !isOurs(c) }, fits) }},
		{"comments.ours.dropped", func() {
			dropOldest(&g.Comments, isOurs, fits)
			dropThreadComments(&g, isOurs, fits)
		}},
		{"threads", func() { g.Threads = nil }},
		{"body.sections", func() { g.Body.Sections = nil }},
		{"reviews", func() { g.Reviews = nil }},
		{"checks.jobs_failed", func() {
			for _, c := range g.Checks {
				c.JobsFailed = nil
			}
		}},
	} {
		before := size()
		if before <= MaxForm {
			break
		}
		trim.do()
		if size() < before { // named only when it cut something
			sy.Trimmed = append(sy.Trimmed, trim.name)
		}
	}
	if n := size(); n > MaxForm {
		sy.Errors = append(sy.Errors, fmt.Sprintf("form %d bytes after every trim (cap %d)", n, MaxForm))
	}
	gj, pj := mustJSON(g.wire()), mustJSON(ps)
	return plugin.FormWrite{Key: key, Set: map[string]json.RawMessage{"gh": gj, "parsed": pj, "sync": mustJSON(sy)}},
		hash12(string(gj) + string(pj) + string(mustJSON(sy.Alarm)) + string(mustJSON(sy.Conflicts))), g
}

// sameJSON: a and b are the same JSON value (key order and spacing aside).
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return string(mustJSON(x)) == string(mustJSON(y))
}

func checkCount(pr *PR) int {
	if len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.Rollup == nil {
		return 0
	}
	return len(pr.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes)
}

func checkTotal(pr *PR) int {
	if len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.Rollup == nil {
		return 0
	}
	return pr.Commits.Nodes[0].Commit.Rollup.Contexts.TotalCount
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// cutAll cuts the bodies of the comments which() picks to SectionHead,
// marked cut; a copy (the caller's map is GitHub's read).
func cutAll(cs map[string]CachedComment, which func(CachedComment) bool) map[string]CachedComment {
	if cs == nil {
		return nil
	}
	out := make(map[string]CachedComment, len(cs))
	for id, c := range cs {
		if which(c) && len(c.Body) > SectionHead {
			c.Body, c.Cut = cut(c.Body, SectionHead), true
		}
		out[id] = c
	}
	return out
}

// deepItem is one entity of a deep read, decoded, before its form is built.
type deepItem struct {
	k     string
	e     *Entity
	pr    *PR
	g     GH
	mark  Mark
	issue bool
	again bool // a long thread of it waits to be read whole: read it again next round
}

// oursThreads reads whole (once per comment count) the review threads longer
// than ThreadLast, keeps our comments in each and merges them into the gh the
// items will write. A failed read keeps the last known ones.
func (s *syncer) oursThreads(items []deepItem) {
	type want struct {
		item *deepItem
		id   string
		n    int
	}
	var need []want
	for i := range items {
		it := &items[i]
		if it.issue {
			continue
		}
		for id, t := range it.g.Threads {
			if t.N <= ThreadLast || !nodeIDRe.MatchString(id) {
				continue
			}
			if c, ok := it.e.OursThreads[id]; ok && c.N == t.N {
				continue
			}
			need = append(need, want{it, id, t.N})
		}
	}
	sort.Slice(need, func(i, j int) bool { return need[i].item.k+need[i].id < need[j].item.k+need[j].id })
	if left := max(MaxFullThreads-s.wholeRead, 0); len(need) > left {
		s.logf("%d long thread(s) to read whole: %d this round, the rest next", len(need), left)
		for _, w := range need[left:] {
			w.item.again = true
		}
		need = need[:left]
	}
	s.wholeRead += len(need)
	if len(need) > 0 {
		var b strings.Builder
		b.WriteString("{")
		for i, w := range need {
			fmt.Fprintf(&b, ` t%d: node(id:"%s"){... on PullRequestReviewThread{comments(first:100){totalCount nodes{%s}}}}`, i, w.id, commentSel)
		}
		b.WriteString("}")
		data, _, err := s.gh.GraphQLPartial(s.ctx, b.String())
		var raw map[string]*struct {
			Comments struct {
				TotalCount int       `json:"totalCount"`
				Nodes      []Comment `json:"nodes"`
			} `json:"comments"`
		}
		if err == nil {
			err = json.Unmarshal(data, &raw)
		}
		if err != nil {
			s.logf("reading %d long thread(s) whole: %v (the last known comments of ours stay)", len(need), err)
		} else {
			for i, w := range need {
				t := raw[fmt.Sprintf("t%d", i)]
				if t == nil {
					continue
				}
				mine := map[string]CachedComment{}
				for id, c := range cached(t.Comments.Nodes) {
					if strings.EqualFold(c.Author, s.p.Ours) {
						mine[id] = c
					}
				}
				if len(mine) == 0 {
					mine = nil
				}
				if w.item.e.OursThreads == nil {
					w.item.e.OursThreads = map[string]OursThread{}
				}
				partial := t.Comments.TotalCount > len(t.Comments.Nodes)
				w.item.e.OursThreads[w.id] = OursThread{N: w.n, Comments: mine, Partial: partial}
				if partial {
					s.logf("%s: thread %s: %d comments, the first %d read whole (gh.threads.%s.partial)", w.item.k, w.id, t.Comments.TotalCount, len(t.Comments.Nodes), w.id)
				}
			}
		}
	}
	for i := range items {
		it := &items[i]
		for id := range it.e.OursThreads {
			if _, ok := it.g.Threads[id]; !ok {
				delete(it.e.OursThreads, id) // the thread is gone (or past the 50 read)
			}
		}
		if len(it.e.OursThreads) == 0 {
			it.e.OursThreads = nil
			continue
		}
		threads := make(map[string]Thread, len(it.g.Threads)) // a copy: the read's map stays GitHub's
		for id, t := range it.g.Threads {
			threads[id] = t
		}
		for id, c := range it.e.OursThreads {
			t := threads[id]
			merged := make(map[string]CachedComment, len(t.Comments)+len(c.Comments))
			for cid, cc := range c.Comments {
				merged[cid] = cc
			}
			for cid, cc := range t.Comments {
				merged[cid] = cc // the newest read is fresher
			}
			if len(merged) == 0 {
				merged = nil
			}
			t.Comments, t.Partial = merged, c.Partial
			threads[id] = t
		}
		it.g.Threads = threads
	}
}

// dropOldest removes the comments of *cs which() picks, oldest first, until
// fits(); *cs becomes a copy first (the caller's map is GitHub's read), so
// fits() measures each removal.
func dropOldest(cs *map[string]CachedComment, which func(CachedComment) bool, fits func() bool) {
	if len(*cs) == 0 || fits() {
		return
	}
	orig := *cs
	out := make(map[string]CachedComment, len(orig))
	var ids []string
	for id, c := range orig {
		out[id] = c
		if which(c) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := orig[ids[i]], orig[ids[j]]
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return ids[i] < ids[j]
	})
	*cs = out
	for _, id := range ids {
		if fits() {
			break
		}
		delete(out, id)
	}
	if len(out) == 0 {
		*cs = nil
	}
}

// dropThreadComments removes the thread comments which() picks, oldest first
// across every thread, until fits().
func dropThreadComments(g *GH, which func(CachedComment) bool, fits func() bool) {
	if fits() {
		return
	}
	type at struct{ thread, id, created string }
	var all []at
	threads := make(map[string]Thread, len(g.Threads))
	for tid, t := range g.Threads {
		cs := make(map[string]CachedComment, len(t.Comments))
		for id, c := range t.Comments {
			cs[id] = c
			if which(c) {
				all = append(all, at{tid, id, c.CreatedAt})
			}
		}
		t.Comments = cs
		threads[tid] = t
	}
	g.Threads = threads
	sort.Slice(all, func(i, j int) bool {
		if all[i].created != all[j].created {
			return all[i].created < all[j].created
		}
		return all[i].thread+all[i].id < all[j].thread+all[j].id
	})
	for _, x := range all {
		if fits() {
			break
		}
		delete(g.Threads[x.thread].Comments, x.id)
	}
	for tid, t := range g.Threads {
		if len(t.Comments) == 0 {
			t.Comments = nil
			g.Threads[tid] = t
		}
	}
}
