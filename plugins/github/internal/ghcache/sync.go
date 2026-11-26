package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// The write path (slice 3, wave 1; DESIGN 2 and 8). A seat changes GitHub by
// editing the entity form: gh.labels, gh.draft, gh.threads.<id>.resolved
// (true), or appending {body} to gh.threads.<id>.replies. Each round the core
// hands the plugin every entity form's gh and sync as it reads them now
// (Request.Owned). Per field the plugin keeps the BASE it last agreed with
// GitHub and saw land on the form (Entity.Base, in ebac's runtime state, never
// on the form) and diffs three ways:
//   - form == base: pull GitHub's value;
//   - form moved, GitHub at base: push, after a re-read right before the write
//     shows GitHub still at the value the merge was computed on (else the write
//     is refused as stale and retried next round against the new value);
//   - both moved: labels merge as a set patch (adds and removes from both
//     sides); a reply is append-only (posted once; a thread gone or resolved
//     upstream is a conflict alarm, the reply kept in sync.conflicts); resolve
//     is idempotent; draft, a bool, cannot disagree when both moved. A push
//     refused, or stale or failed MaxTries rounds, leaves GitHub's value and
//     puts the form's in sync.conflicts.<field> with an alarm.
// Nothing is written outside the trusted scope (Pass.Auth: model.AuthorizeGHWrite).
// Every failure it cannot settle is sync.alarm.<kind>.<field> on the entity
// form and ONE wake of the trusted maintainer (Pass.Maintainer).

// MaxWrites bounds one round's GitHub writes: past it, wishes wait on the
// form for the next round.
const MaxWrites = 20

// MaxAlarmPings: more new alarms than this in one round are woken as one
// collect ping, not one each.
const MaxAlarmPings = 3

// MaxSent bounds the reply hashes kept per entity (already posted: never
// posted again).
const MaxSent = 50

// MaxTries: a push that was stale or failed this many rounds in a row is
// given up with a write-refused alarm (GitHub's value stands, the form's goes
// to sync.conflicts).
const MaxTries = 2

// Alarm kinds (DESIGN 8, wave 1).
const (
	AlarmConflict     = "conflict"
	AlarmWriteRefused = "write-refused"
	AlarmUnparseable  = "unparseable"
)

// Base is what a PR's write fields were when the sync last agreed with
// GitHub and that agreement landed on the form.
type Base struct {
	Labels   []string `json:"labels"`
	Draft    bool     `json:"draft"`
	Resolved []string `json:"resolved,omitempty"` // ids of the threads resolved
	// Comments: comment node id -> hash12 of its body as agreed (comment_edit).
	Comments map[string]string `json:"comments,omitempty"`
}

// Agreed is a round's base and read-only hash, kept pending until the write
// carrying them lands (forms_landed): a form that never received a write is
// never diffed against it. Pushed are the write fields this round's own
// GitHub writes set: GitHub holds them whether the form write lands or not,
// so they become the base either way (a seat that edits the form back
// before it lands has asked for a new write, not been pulled over).
type Agreed struct {
	Base   Base    `json:"base"`
	RO     string  `json:"ro"`
	Pushed *Pushed `json:"pushed,omitempty"`
}

// Pushed are the write fields a round's GitHub writes set.
// Labels are the labels this round added and removed, never GitHub's whole
// set: a label added upstream meanwhile is not ours to promote.
type Pushed struct {
	Added    []string          `json:"added,omitempty"`
	Removed  []string          `json:"removed,omitempty"`
	Draft    *bool             `json:"draft,omitempty"`
	Resolved []string          `json:"resolved,omitempty"`
	Comments map[string]string `json:"comments,omitempty"` // id -> hash12 of the body written
}

// onto is b with the pushed fields applied.
func (p *Pushed) onto(b Base) Base {
	if p == nil {
		return b
	}
	if len(p.Added)+len(p.Removed) > 0 {
		b.Labels = sortedSet(append(minus(b.Labels, p.Removed), p.Added...))
	}
	if p.Draft != nil {
		b.Draft = *p.Draft
	}
	if len(p.Resolved) > 0 {
		b.Resolved = sortedSet(append(slices.Clone(b.Resolved), p.Resolved...))
	}
	if len(p.Comments) > 0 {
		c := maps.Clone(b.Comments)
		if c == nil {
			c = map[string]string{}
		}
		maps.Copy(c, p.Comments)
		b.Comments = c
	}
	return b
}

func (e *Entity) pushed() *Pushed {
	if e.pushing == nil {
		e.pushing = &Pushed{}
	}
	return e.pushing
}

// Alarm is one sync.alarm.<kind>.<field> (DESIGN 8). Ack is the seat's: set
// it true to clear the alarm.
type Alarm struct {
	At      string `json:"at"`
	Detail  string `json:"detail"`
	BaseRef string `json:"base_ref,omitempty"`
	FormRef string `json:"form_ref,omitempty"`
	GHRef   string `json:"gh_ref,omitempty"`
	Ack     bool   `json:"ack,omitempty"`
}

// formView is one entity form as the core read it this round.
type formView struct {
	want           *wantView // want.* (create.go); never nil
	ghRaw, syncRaw json.RawMessage
	gh             *GH   // nil when absent or unparseable
	bad            error // gh did not parse
	unknown        []string
	acks           []string // alarm keys (kind.field) the seat acked
}

var ghKnown = func() map[string]bool {
	m := map[string]bool{}
	t := reflect.TypeOf(GH{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		m[name] = true
	}
	return m
}()

func readForm(owned map[string]json.RawMessage) *formView {
	fv := &formView{ghRaw: owned["gh"], syncRaw: owned["sync"], want: readWant(owned["want"])}
	if len(fv.ghRaw) > 0 {
		var g GH
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(fv.ghRaw, &keys); err != nil {
			fv.bad = err
		} else if err := json.Unmarshal(fv.ghRaw, &g); err != nil {
			fv.bad = err
		} else {
			fv.gh = &g
			for k := range keys {
				if !ghKnown[k] {
					fv.unknown = append(fv.unknown, k)
				}
			}
			sort.Strings(fv.unknown)
		}
	}
	var sy struct {
		Alarm map[string]map[string]struct {
			Ack bool `json:"ack"`
		} `json:"alarm"`
	}
	if json.Unmarshal(fv.syncRaw, &sy) == nil {
		for kind, fs := range sy.Alarm {
			for f, a := range fs {
				if a.Ack {
					fv.acks = append(fv.acks, kind+"."+f)
				}
			}
		}
		sort.Strings(fv.acks)
	}
	return fv
}

// MaxBaseOthers and MaxBaseOurs bound the comment hashes one entity's base
// keeps (#71s, live 11-25: a hash for every comment of 44 open PRs, kept
// twice once a write lands (base and pend), grew the critic form past
// figaro's state budget and every write was refused). Ours are the only ones
// ever edited; another author's hash only lets a seat's edit of that text be
// named as such (past the bound the edit is reverted to GitHub's text
// without the alarm). The newest of each are kept.
const (
	MaxBaseOthers = 20
	MaxBaseOurs   = 50
)

// AlarmOverBound: more of our comments than MaxBaseOurs on one entity; the
// ones past the bound have no base, so an edit of them is not written.
const AlarmOverBound = "over-bound"

// boundBase cuts b's comment hashes to the bounds (authors from g) and
// raises, or clears, the over-bound alarm for our comments left out.
func (s *syncer) boundBase(k string, e *Entity, b *Base, g *GH) {
	key := AlarmOverBound + ".comments"
	if len(b.Comments) <= MaxBaseOthers {
		s.overLeave(k, e, "comments", OverLeaveReads) // under the bound
		return
	}
	all := allComments(g)
	var mine, others []string
	for id := range b.Comments {
		if s.p.Ours != "" && strings.EqualFold(all[id].Author, s.p.Ours) {
			mine = append(mine, id)
		} else {
			others = append(others, id)
		}
	}
	newest := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool {
			if ta, tc := all[ids[i]].CreatedAt, all[ids[j]].CreatedAt; ta != tc {
				return ta > tc // newest first
			}
			return ids[i] < ids[j]
		})
	}
	newest(mine)
	newest(others)
	for _, id := range others[min(len(others), MaxBaseOthers):] {
		delete(b.Comments, id)
	}
	lost := mine[min(len(mine), MaxBaseOurs):]
	for _, id := range lost {
		delete(b.Comments, id)
	}
	if len(lost) == 0 {
		s.overLeave(k, e, "comments", OverLeaveReads) // none of ours left out
		return
	}
	s.overEnter(k, e, "comments", fmt.Sprintf("%d of our comments are past the base bound (the newest %d of ours are kept): an edit of those is not written", len(lost), MaxBaseOurs))
}

// overEnter: k is over the bound for field this read. The alarm is raised
// (one wake) only on entering an episode; within one, a clean count restarts
// and nothing is raised, also after a seat's ack (#71v).
func (s *syncer) overEnter(k string, e *Entity, field, detail string) {
	if o := e.Over[field]; o != nil {
		o.Clean, o.Last = 0, s.now
		return
	}
	if e.Over == nil {
		e.Over = map[string]*OverEpisode{}
	}
	e.Over[field] = &OverEpisode{Since: s.now, Last: s.now}
	s.alarm(k, e, AlarmOverBound, field, detail, nil, nil, nil) // an alarm standing from before episodes were kept is not raised twice
}

// overLeave: k is within the bound for field this read. An episode ends
// after need clean reads in a row: its alarm clears and the maintainer is
// pinged once. An alarm with no episode (raised before they were kept)
// clears quietly.
func (s *syncer) overLeave(k string, e *Entity, field string, need int) {
	key := AlarmOverBound + "." + field
	o := e.Over[field]
	if o == nil {
		delete(e.Alarms, key)
		return
	}
	o.Clean++
	if o.Clean < need {
		return
	}
	delete(e.Over, field)
	if len(e.Over) == 0 {
		e.Over = nil
	}
	delete(e.Alarms, key)
	s.cleared = append(s.cleared, fmt.Sprintf("%s %s: cleared: back within the bound (over since %s)", k, key, o.Since.UTC().Format(time.RFC3339)))
}

// AlarmEndedEdit: a seat edited a write field on an ended entity's form (a
// read-only mirror, #71s); the edit is reverted by the write that raises it.
const AlarmEndedEdit = "ended-edit"

// mirrorOf is g's write fields without comment hashes (Entity.Ended).
func mirrorOf(g *GH) *Base {
	b := baseOf(g)
	b.Comments = nil
	return &b
}

// endedEdits raises one ended-edit alarm per write field a seat moved on an
// ended entity's form F (G is GitHub's, which this write puts back): labels,
// draft and a thread's resolved against the form as last written
// (Entity.Ended; none known, none judged), every reply asked, and a comment
// body that differs from GitHub's while GitHub's updated_at is the one the
// form shows (GitHub did not move it; a cut body is never an edit).
func (s *syncer) endedEdits(k string, e *Entity, F, G *GH) {
	const why = "the entity ended (closed, merged or no longer tracked): its form is a read-only mirror, the edit is reverted and not written"
	if m := e.Ended; m != nil {
		if F.Draft != m.Draft {
			s.alarm(k, e, AlarmEndedEdit, "draft", why, m.Draft, F.Draft, G.Draft)
		}
		if fl := sortedSet(F.Labels); !slices.Equal(fl, m.Labels) {
			s.alarm(k, e, AlarmEndedEdit, "labels", why, m.Labels, fl, G.Labels)
		}
	}
	ids := slices.Sorted(maps.Keys(F.Threads))
	for _, id := range ids {
		t := F.Threads[id]
		if m := e.Ended; m != nil && t.Resolved != slices.Contains(m.Resolved, id) {
			s.alarm(k, e, AlarmEndedEdit, "thread-"+id+"-resolved", why, !t.Resolved, t.Resolved, nil)
		}
		if len(t.Replies) > 0 {
			s.alarm(k, e, AlarmEndedEdit, "thread-"+id+"-replies", why, nil, t.Replies, nil)
		}
	}
	gc := allComments(G)
	fc := allComments(F)
	for _, id := range slices.Sorted(maps.Keys(fc)) {
		f := fc[id]
		g, ok := gc[id]
		if !ok || f.Cut || g.Cut || f.UpdatedAt != g.UpdatedAt || f.Body == g.Body {
			continue
		}
		s.alarm(k, e, AlarmEndedEdit, commentField(id), why, nil, f.Body, g.Body)
	}
}

func baseOf(g *GH) Base {
	b := Base{Labels: sortedSet(g.Labels), Draft: g.Draft}
	for id, t := range g.Threads {
		if t.Resolved {
			b.Resolved = append(b.Resolved, id)
		}
	}
	sort.Strings(b.Resolved)
	for id, c := range allComments(g) {
		if b.Comments == nil {
			b.Comments = map[string]string{}
		}
		b.Comments[id] = hash12(c.Body)
	}
	return b
}

// roOf is gh without its write fields (labels, draft, thread resolved and
// replies, comment bodies), for the read-only check.
func roOf(g *GH) GH {
	c := *g
	c.Labels, c.Draft = nil, false
	c.Comments = noBodies(g.Comments)
	if g.Threads != nil {
		c.Threads = map[string]Thread{}
		for id, t := range g.Threads {
			t.Resolved, t.Replies = false, nil
			t.Comments = noBodies(t.Comments)
			c.Threads[id] = t
		}
	}
	return c
}

func roHash(g *GH) string { return hash12(string(mustJSON(roOf(g)))) }

// pending: the form asks for a write (a write field differs from the base or
// a reply waits).
func pending(f *GH, b *Base) bool {
	if f.Draft != b.Draft || !slices.Equal(sortedSet(f.Labels), b.Labels) {
		return true
	}
	for id, t := range f.Threads {
		if len(t.Replies) > 0 || t.Resolved != slices.Contains(b.Resolved, id) {
			return true
		}
	}
	for id, c := range allComments(f) {
		if h, ok := b.Comments[id]; ok && !c.Cut && h != hash12(c.Body) {
			return true // a cut body is a head of the text, never an edit
		}
	}
	return false
}

func sortedSet(s []string) []string {
	out := []string{}
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func minus(a, b []string) []string {
	out := []string{}
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// ref is a short hash of a value (an alarm's base/form/gh ref).
func ref(v any) string {
	if v == nil {
		return ""
	}
	return hash12(string(mustJSON(v)))
}

var (
	nodeIDRe      = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,100}$`)
	threadAliasRe = regexp.MustCompile(`^t\d+$`)
)

// syncer is one round's write path.
type syncer struct {
	ctx       context.Context
	gh        GitHub
	p         Pass
	now       time.Time
	writes    int
	raised    []string // this round's new alarms: "<key> <kind>.<field>: detail"
	cleared   []string // this round's ended over-bound episodes, the same shape (#71v)
	logf      func(string, ...any)
	children  map[string]string
	cur       string     // the entity being synced (telemetry)
	log       []WriteRec // this round's GitHub writes (telemetry)
	wholeRead int        // long threads read whole this round (MaxFullThreads)
	inflight  int        // bytes of creates recorded and not done (create.go: MaxInFlightBytes)
}

func (s *syncer) alarm(k string, e *Entity, kind, field, detail string, base, form, gh any) {
	key := kind + "." + field
	if e.Alarms == nil {
		e.Alarms = map[string]Alarm{}
	}
	if _, had := e.Alarms[key]; had {
		return // one wake per alarm: it stands until fixed or acked
	}
	e.Alarms[key] = Alarm{At: s.now.Format(time.RFC3339), Detail: cut(detail, 300), BaseRef: ref(base), FormRef: ref(form), GHRef: ref(gh)}
	s.raised = append(s.raised, fmt.Sprintf("%s %s: %s", k, key, detail))
}

// settled clears field's conflict and write-refused alarms (a later clean
// push of it) and its conflict value.
func settled(e *Entity, field string) {
	delete(e.Alarms, AlarmConflict+"."+field)
	delete(e.Alarms, AlarmWriteRefused+"."+field)
	delete(e.Conflicts, field)
	delete(e.Tries, field)
}

func (s *syncer) conflictValue(e *Entity, field string, v any) {
	if e.Conflicts == nil {
		e.Conflicts = map[string]json.RawMessage{}
	}
	e.Conflicts[field] = mustJSON(v)
}

// retry counts one more stale or failed attempt at field and says whether
// the push is given up (MaxTries reached).
func retry(e *Entity, field string) bool {
	if e.Tries == nil {
		e.Tries = map[string]int{}
	}
	e.Tries[field]++
	return e.Tries[field] >= MaxTries
}

// replyWant is one reply to post.
type replyWant struct {
	thread, body, hash string
}

// fresh is GitHub's state of a PR's write fields read right before its
// writes (preWriteQuery).
type fresh struct {
	draft       bool
	labels      map[string]string // name -> id
	labelsTotal int
	repoLabels  map[string]string // name -> id, for the labels to add
	threads     map[string]*freshThread
}

type freshThread struct {
	resolved bool
	mine     []string // bodies of the newest comments the operator wrote
}

func preWriteQuery(repo, node string, threads, add []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{ pre: node(id:"%s"){... on PullRequest{isDraft labels(first:100){totalCount nodes{id name}}}}`, node)
	for i, t := range threads {
		fmt.Fprintf(&b, ` t%d: node(id:"%s"){... on PullRequestReviewThread{isResolved comments(last:10){nodes{body viewerDidAuthor}}}}`, i, t)
	}
	if len(add) > 0 {
		o, n := splitRepo(repo)
		fmt.Fprintf(&b, ` repo: repository(owner:"%s",name:"%s"){`, o, n)
		for i, l := range add {
			fmt.Fprintf(&b, ` l%d: label(name:%s){id name}`, i, mustJSON(l))
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

func (s *syncer) readFresh(repo, node string, threads, add []string) (*fresh, error) {
	data, errs, err := s.gh.GraphQLPartial(s.ctx, preWriteQuery(repo, node, threads, add))
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		// A missing label is a null label and a missing thread a null node
		// (both NOT_FOUND on their alias), not a failed read; anything else is.
		if (len(e.Path) >= 2 && e.Path[0] == "repo") || (len(e.Path) == 1 && threadAliasRe.MatchString(e.Path[0]) && e.Type == "NOT_FOUND") {
			continue
		}
		return nil, fmt.Errorf("%s %s at %s", e.Type, e.Message, strings.Join(e.Path, "."))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var pr *struct {
		IsDraft bool `json:"isDraft"`
		Labels  struct {
			TotalCount int `json:"totalCount"`
			Nodes      []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw["pre"], &pr); err != nil || pr == nil {
		return nil, fmt.Errorf("the PR (%s) did not answer", node)
	}
	f := &fresh{draft: pr.IsDraft, labels: map[string]string{}, labelsTotal: pr.Labels.TotalCount, repoLabels: map[string]string{}, threads: map[string]*freshThread{}}
	for _, l := range pr.Labels.Nodes {
		f.labels[l.Name] = l.ID
	}
	for i, id := range threads {
		var t *struct {
			IsResolved bool `json:"isResolved"`
			Comments   struct {
				Nodes []struct {
					Body string `json:"body"`
					Mine bool   `json:"viewerDidAuthor"`
				} `json:"nodes"`
			} `json:"comments"`
		}
		if json.Unmarshal(raw[fmt.Sprintf("t%d", i)], &t) != nil || t == nil {
			continue // gone
		}
		ft := &freshThread{resolved: t.IsResolved}
		for _, c := range t.Comments.Nodes {
			if c.Mine {
				ft.mine = append(ft.mine, c.Body)
			}
		}
		f.threads[id] = ft
	}
	var rl map[string]*struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if len(raw["repo"]) > 0 && json.Unmarshal(raw["repo"], &rl) == nil {
		for _, l := range rl {
			if l != nil {
				f.repoLabels[l.Name] = l.ID
			}
		}
	}
	return f, nil
}

// mutate sends one write, counted against MaxWrites, and records it.
func (s *syncer) mutate(q string, vars map[string]string) (json.RawMessage, error) {
	s.writes++
	data, err := s.gh.Mutate(s.ctx, q, vars)
	w := WriteRec{PR: s.cur, Mutation: mutationName(q), OK: err == nil}
	if err != nil {
		w.Error = cut(err.Error(), 200)
	}
	s.log = append(s.log, w)
	return data, err
}

func quoteIDs(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = `"` + id + `"`
	}
	return strings.Join(q, ",")
}

// refusedByGitHub: a write GitHub answered with a refusal no retry changes
// (a GraphQL error beside the data, or an HTTP 4xx but 429).
func refusedByGitHub(err error) bool {
	return strings.HasPrefix(err.Error(), "mutation: ") || forge.ClientError(err) != ""
}

// entity runs the three-way sync of one deep-read PR. g is GitHub's state as
// read this round; it returns the gh to write (g with the agreed and still
// pending write fields), sets e.Pend.Base to the base that write carries
// (the caller sets e.Pend.RO from the gh as written), and returns notes for
// sync.errors.
func (s *syncer) entity(k string, e *Entity, pr *PR, g GH, fv *formView) (GH, []string) {
	s.cur = k
	var notes []string
	G := g // GitHub's, untouched
	out := g
	out.Labels = slices.Clone(g.Labels)
	if g.Threads != nil {
		out.Threads = map[string]Thread{}
		for id, t := range g.Threads {
			out.Threads[id] = t
		}
	}
	if fv != nil && fv.gh != nil {
		delete(e.Alarms, AlarmUnparseable+".gh") // the form parses again
	}
	if fv == nil || fv.gh == nil || e.Base == nil {
		if fv != nil && fv.bad != nil {
			s.alarm(k, e, AlarmUnparseable, "gh", "gh on the form is not a GitHub cache object ("+fv.bad.Error()+"): rewritten from GitHub", nil, string(fv.ghRaw), nil)
		}
		if fv != nil && fv.gh != nil && e.Base == nil && !e.EndedAt.IsZero() {
			s.endedEdits(k, e, fv.gh, &g)
		}
		b := baseOf(&out)
		s.boundBase(k, e, &b, &out)
		e.Pend = &Agreed{Base: b}
		e.shown = mirrorOf(&out)
		return out, notes
	}
	F, B := fv.gh, *e.Base
	next := baseOf(&G)
	ent := model.GHEntity{Repo: e.Repo, Kind: model.FirstNonEmpty(e.Kind, KindPR), N: e.N, Author: g.Author}
	auth := func(field string) string {
		if e.Kind == KindIssue && field != model.FieldCommentEdit {
			// #45 reads issues; their other writes (title, body, labels)
			// come with the issue write path (#71 A2).
			return "issue writes are not built yet: the form's edit is not written"
		}
		if s.p.Auth == nil {
			return "no write authority"
		}
		if e.Kind == KindIssue {
			ie := ent
			ie.Kind = KindIssue
			return s.p.Auth(ie, field)
		}
		return s.p.Auth(ent, field)
	}

	// Read-only fields: a form edit is reverted (the write below is GitHub's).
	if e.RO != "" && roHash(F) != e.RO {
		var names []string
		fm, gm := map[string]json.RawMessage{}, map[string]json.RawMessage{}
		_ = json.Unmarshal(mustJSON(roOf(F)), &fm)
		_ = json.Unmarshal(mustJSON(roOf(&G)), &gm)
		for name, v := range fm {
			if string(v) != string(gm[name]) {
				names = append(names, "gh."+name)
			}
		}
		sort.Strings(names)
		notes = append(notes, fmt.Sprintf("read-only %s edited on the form: reverted to GitHub's", model.FirstNonEmpty(strings.Join(names, ", "), "fields")))
	}
	if len(fv.unknown) > 0 {
		notes = append(notes, fmt.Sprintf("gh.%s: not a GitHub cache field: removed", strings.Join(fv.unknown, ", gh.")))
	}

	// 1. Decide each write field.
	type push struct {
		field, wfield string // alarm field, model write field
	}
	var draftWant *bool
	// draft is a bool: when both sides moved they agree, so it never
	// conflicts; the form moved alone is a push.
	if F.Draft != B.Draft && F.Draft != G.Draft {
		if why := auth(model.FieldDraft); why != "" {
			s.alarm(k, e, AlarmWriteRefused, "draft", why, B.Draft, F.Draft, G.Draft)
		} else {
			v := F.Draft
			draftWant = &v
		}
	}
	fl, gl := sortedSet(F.Labels), sortedSet(G.Labels)
	var addL, rmL, merged []string
	if !slices.Equal(fl, B.Labels) {
		merged = sortedSet(append(minus(gl, minus(B.Labels, fl)), minus(fl, B.Labels)...))
		addL, rmL = minus(merged, gl), minus(gl, merged)
		if len(addL)+len(rmL) > 0 {
			switch {
			case pr.Labels.TotalCount > len(pr.Labels.Nodes):
				s.alarm(k, e, AlarmWriteRefused, "labels", fmt.Sprintf("the PR has %d labels, more than the %d read: a label write could drop the rest", pr.Labels.TotalCount, len(pr.Labels.Nodes)), B.Labels, fl, gl)
				addL, rmL = nil, nil
			default:
				if why := auth(model.FieldLabels); why != "" {
					s.alarm(k, e, AlarmWriteRefused, "labels", why, B.Labels, fl, gl)
					addL, rmL = nil, nil
				}
			}
		}
	}
	var resolveWant []string
	var replies []replyWant
	keep := map[string][]Reply{} // replies left on the form for the next round
	threadIDs := make([]string, 0, len(F.Threads))
	for id := range F.Threads {
		threadIDs = append(threadIDs, id)
	}
	sort.Strings(threadIDs)
	for _, id := range threadIDs {
		ft := F.Threads[id]
		bt := slices.Contains(B.Resolved, id)
		gt, onGH := G.Threads[id]
		rf := "thread-" + id + "-resolved"
		switch {
		case ft.Resolved == bt || (onGH && ft.Resolved == gt.Resolved):
		case !nodeIDRe.MatchString(id):
			s.alarm(k, e, AlarmUnparseable, "threads", fmt.Sprintf("thread id %q is not a node id", cut(id, 40)), nil, id, nil)
		case !onGH:
			s.alarm(k, e, AlarmWriteRefused, rf, "the thread is not on the PR as read (deleted, or past the threads read)", bt, ft.Resolved, nil)
		case ft.Resolved && !gt.Resolved:
			if why := auth(model.FieldThreadResolve); why != "" {
				s.alarm(k, e, AlarmWriteRefused, rf, why, bt, true, false)
			} else {
				resolveWant = append(resolveWant, id)
			}
		default: // unresolve: not a wave-1 write
			notes = append(notes, fmt.Sprintf("gh.threads.%s.resolved: unresolving is not a write the cache makes: reverted", id))
		}
		rpf := "thread-" + id + "-replies"
		for _, r := range ft.Replies {
			h := hash12(id + "\n" + r.Body)
			switch {
			case slices.Contains(e.Sent, h):
				// posted already: dropped from the form
			case strings.TrimSpace(r.Body) == "":
				s.alarm(k, e, AlarmUnparseable, rpf, "a reply with an empty body: dropped", nil, r, nil)
			case !nodeIDRe.MatchString(id):
				s.alarm(k, e, AlarmUnparseable, "threads", fmt.Sprintf("thread id %q is not a node id", cut(id, 40)), nil, id, nil)
			case !onGH:
				s.alarm(k, e, AlarmWriteRefused, rpf, "the thread is not on the PR as read (deleted, or past the threads read): reply not posted (in sync.conflicts)", nil, r, nil)
				s.conflictValue(e, rpf, r)
			case gt.Resolved && !bt:
				s.alarm(k, e, AlarmConflict, rpf, "the thread was resolved on GitHub since the base: reply not posted (in sync.conflicts)", bt, r, gt.Resolved)
				s.conflictValue(e, rpf, r)
			default:
				if why := auth(model.FieldThreadReply); why != "" {
					s.alarm(k, e, AlarmWriteRefused, rpf, why, nil, r, nil)
				} else if why := disclose.Disclosure(r.Body); why != "" {
					s.alarm(k, e, AlarmWriteRefused, rpf, why, nil, r, nil)
					s.conflictValue(e, rpf, r)
				} else {
					replies = append(replies, replyWant{id, r.Body, h})
				}
			}
		}
	}

	// What stays on the form when a wish waits (stale, failed, past the cap):
	// the form's value, against the old base.
	waitDraft := func() { out.Draft, next.Draft = F.Draft, B.Draft }
	waitLabels := func() { out.Labels, next.Labels = slices.Clone(fl), slices.Clone(B.Labels) }
	waitResolve := func(id string) { // the base (unresolved) stays; the form keeps its true
		if t, ok := out.Threads[id]; ok {
			t.Resolved = true
			out.Threads[id] = t
		}
	}
	waitReply := func(r replyWant) { keep[r.thread] = append(keep[r.thread], Reply{Body: r.body}) }
	giveUp := func(field, detail string, form any) {
		s.alarm(k, e, AlarmWriteRefused, field, detail, nil, form, nil)
		s.conflictValue(e, field, form)
	}

	nWrites := len(addL) + len(rmL) + len(resolveWant) + len(replies)
	if draftWant != nil {
		nWrites++
	}
	if nWrites > 0 {
		if s.writes+nWrites > MaxWrites || !nodeIDRe.MatchString(pr.ID) {
			if !nodeIDRe.MatchString(pr.ID) {
				notes = append(notes, "the PR's node id was not read: writes wait")
			} else {
				s.logf("%s: %d write(s) wait for the next round (cap %d per round)", k, nWrites, MaxWrites)
			}
			if draftWant != nil {
				waitDraft()
			}
			if len(addL)+len(rmL) > 0 {
				waitLabels()
			}
			for _, id := range resolveWant {
				waitResolve(id)
			}
			for _, r := range replies {
				waitReply(r)
			}
		} else {
			var tids []string
			for _, id := range resolveWant {
				tids = append(tids, id)
			}
			for _, r := range replies {
				if !slices.Contains(tids, r.thread) {
					tids = append(tids, r.thread)
				}
			}
			fr, err := s.readFresh(e.Repo, pr.ID, tids, addL)
			if err != nil {
				s.logf("%s: re-read before the write failed: %v (the writes wait)", k, err)
				if draftWant != nil {
					waitDraft()
				}
				if len(addL)+len(rmL) > 0 {
					waitLabels()
				}
				for _, id := range resolveWant {
					waitResolve(id)
				}
				for _, r := range replies {
					waitReply(r)
				}
			} else {
				s.apply(k, e, pr, fr, &out, &next, G, fl, gl, merged, addL, rmL, draftWant, resolveWant, replies, waitDraft, waitLabels, waitResolve, waitReply, giveUp)
			}
		}
	}
	if out.Threads != nil {
		for id, rs := range keep {
			if t, ok := out.Threads[id]; ok {
				t.Replies = rs
				out.Threads[id] = t
			}
		}
	}
	s.commentEdits(k, e, G, F, B, &out, &next, auth)
	s.boundBase(k, e, &next, &G)
	e.Pend = &Agreed{Base: next, Pushed: e.pushing} // RO: the caller's, from the gh as written
	e.pushing = nil
	e.shown = mirrorOf(&out)
	return out, notes
}

// apply makes the writes the fresh read still allows.
func (s *syncer) apply(k string, e *Entity, pr *PR, fr *fresh, out *GH, next *Base, G GH, fl, gl, merged, addL, rmL []string,
	draftWant *bool, resolveWant []string, replies []replyWant, waitDraft, waitLabels func(), waitResolve func(string), waitReply func(replyWant),
	giveUp func(field, detail string, form any)) {
	if draftWant != nil {
		switch {
		case fr.draft != G.Draft:
			if retry(e, "draft") {
				giveUp("draft", "GitHub's draft moved between the read and the write, twice: GitHub's stands, the form's is in sync.conflicts.draft", *draftWant)
			} else {
				s.logf("%s: draft: GitHub moved since the read (stale base): not written, retried next round", k)
				waitDraft()
			}
		default:
			q := fmt.Sprintf(`mutation{markPullRequestReadyForReview(input:{pullRequestId:"%s"}){pullRequest{isDraft}}}`, pr.ID)
			if *draftWant {
				q = fmt.Sprintf(`mutation{convertPullRequestToDraft(input:{pullRequestId:"%s"}){pullRequest{isDraft}}}`, pr.ID)
			}
			if _, err := s.mutate(q, nil); err != nil {
				if refusedByGitHub(err) || retry(e, "draft") {
					giveUp("draft", "GitHub refused the draft write: "+err.Error(), *draftWant)
				} else {
					s.logf("%s: draft write failed: %v (retried next round)", k, err)
					waitDraft()
				}
			} else {
				out.Draft, next.Draft = *draftWant, *draftWant
				v := *draftWant
				e.pushed().Draft = &v
				settled(e, "draft")
				s.logf("%s: draft set %v", k, *draftWant)
			}
		}
	}
	if len(addL)+len(rmL) > 0 {
		have := make([]string, 0, len(fr.labels))
		for n := range fr.labels {
			have = append(have, n)
		}
		switch {
		case fr.labelsTotal > len(fr.labels) || !slices.Equal(sortedSet(have), gl):
			if retry(e, "labels") {
				giveUp("labels", "GitHub's labels moved between the read and the write, twice: GitHub's stand, the form's are in sync.conflicts.labels", fl)
			} else {
				s.logf("%s: labels: GitHub moved since the read (stale base): not written, retried next round", k)
				waitLabels()
			}
		default:
			var addIDs, missing []string
			for _, l := range addL {
				if id, ok := fr.repoLabels[l]; ok && nodeIDRe.MatchString(id) {
					addIDs = append(addIDs, id)
				} else {
					missing = append(missing, l)
				}
			}
			var rmIDs, noID []string
			for _, l := range rmL {
				if id := fr.labels[l]; nodeIDRe.MatchString(id) {
					rmIDs = append(rmIDs, id)
				} else {
					noID = append(noID, l)
				}
			}
			if len(missing) > 0 {
				giveUp("labels", fmt.Sprintf("label(s) %s do not exist in %s: nothing written", strings.Join(missing, ", "), e.Repo), fl)
				break
			}
			if len(noID) > 0 {
				giveUp("labels", fmt.Sprintf("label(s) %s have no node id in GitHub's answer: nothing written", strings.Join(noID, ", ")), fl)
				break
			}
			var err error
			if len(addIDs) > 0 {
				_, err = s.mutate(fmt.Sprintf(`mutation{addLabelsToLabelable(input:{labelableId:"%s",labelIds:[%s]}){clientMutationId}}`, pr.ID, quoteIDs(addIDs)), nil)
			}
			if err == nil && len(rmIDs) > 0 {
				_, err = s.mutate(fmt.Sprintf(`mutation{removeLabelsFromLabelable(input:{labelableId:"%s",labelIds:[%s]}){clientMutationId}}`, pr.ID, quoteIDs(rmIDs)), nil)
			}
			switch {
			case err == nil:
				out.Labels, next.Labels = slices.Clone(merged), slices.Clone(merged)
				e.pushed().Added, e.pushed().Removed = slices.Clone(addL), slices.Clone(rmL)
				settled(e, "labels")
				s.logf("%s: labels +%v -%v", k, addL, rmL)
			case refusedByGitHub(err) || retry(e, "labels"):
				giveUp("labels", "GitHub refused the label write: "+err.Error(), fl)
			default:
				// An add may have landed before the remove failed: the next
				// round's three-way sees it as GitHub's (both moved alike).
				s.logf("%s: label write failed: %v (retried next round)", k, err)
				waitLabels()
			}
		}
	}
	for _, id := range resolveWant {
		f := "thread-" + id + "-resolved"
		t := fr.threads[id]
		switch {
		case t == nil:
			giveUp(f, "the thread is gone from GitHub", true)
		case t.resolved: // resolved meanwhile: idempotent
			setResolved(out, next, id)
			settled(e, f)
		default:
			_, err := s.mutate(fmt.Sprintf(`mutation{resolveReviewThread(input:{threadId:"%s"}){thread{isResolved}}}`, id), nil)
			switch {
			case err == nil:
				setResolved(out, next, id)
				e.pushed().Resolved = append(e.pushed().Resolved, id)
				settled(e, f)
				s.logf("%s: thread %s resolved", k, id)
			case refusedByGitHub(err) || retry(e, f):
				giveUp(f, "GitHub refused the resolve: "+err.Error(), true)
			default:
				s.logf("%s: resolve %s failed: %v (retried next round)", k, id, err)
				waitResolve(id)
			}
		}
	}
	for _, r := range replies {
		f := "thread-" + r.thread + "-replies"
		t := fr.threads[r.thread]
		switch {
		case t == nil:
			giveUp(f, "the thread is gone from GitHub: reply not posted (in sync.conflicts)", Reply{Body: r.body})
		case slices.Contains(t.mine, r.body):
			e.Sent = appendSent(e.Sent, r.hash) // posted before (a round that never saved): not again
		case t.resolved && !G.Threads[r.thread].Resolved:
			s.alarm(k, e, AlarmConflict, f, "the thread was resolved on GitHub between the read and the write: reply not posted (in sync.conflicts)", nil, Reply{Body: r.body}, true)
			s.conflictValue(e, f, Reply{Body: r.body})
		default:
			data, err := s.mutate(`mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{state url}}}`,
				map[string]string{"t": r.thread, "b": r.body})
			var ans struct {
				Reply struct {
					Comment *struct {
						State string `json:"state"`
						URL   string `json:"url"`
					} `json:"comment"`
				} `json:"addPullRequestReviewThreadReply"`
			}
			if err == nil {
				if json.Unmarshal(data, &ans) != nil || ans.Reply.Comment == nil {
					err = fmt.Errorf("unreadable answer %s", cut(string(data), 120))
				}
			}
			switch {
			case err == nil && ans.Reply.Comment.State == "SUBMITTED":
				e.Sent = appendSent(e.Sent, r.hash)
				settled(e, f)
				s.logf("%s: replied on %s: %s", k, r.thread, ans.Reply.Comment.URL)
			case err == nil:
				// Absorbed into the operator's pending review: on GitHub, not
				// published. Never posted again; the maintainer decides.
				e.Sent = appendSent(e.Sent, r.hash)
				s.alarm(k, e, AlarmWriteRefused, f, fmt.Sprintf("the reply landed %s (a pending review absorbed it), not published: %s", ans.Reply.Comment.State, ans.Reply.Comment.URL), nil, Reply{Body: r.body}, nil)
			case refusedByGitHub(err) || retry(e, f):
				giveUp(f, "GitHub refused the reply: "+err.Error(), Reply{Body: r.body})
			default:
				s.logf("%s: reply on %s failed: %v (retried next round)", k, r.thread, err)
				waitReply(r)
			}
		}
	}
}

func setResolved(out *GH, next *Base, id string) {
	if t, ok := out.Threads[id]; ok {
		t.Resolved = true
		out.Threads[id] = t
	}
	next.Resolved = sortedSet(append(next.Resolved, id))
}

func appendSent(s []string, h string) []string {
	s = append(s, h)
	if len(s) > MaxSent {
		s = s[len(s)-MaxSent:]
	}
	return s
}

// pings are the round's wakes of the trusted maintainer: one per new alarm,
// or one collect ping past MaxAlarmPings.
func (s *syncer) pings(critic string) []plugin.Ping {
	if len(s.raised)+len(s.cleared) == 0 {
		return nil
	}
	if s.p.Maintainer == "" {
		all := append(slices.Clone(s.raised), s.cleared...)
		s.logf("%d new sync alarm(s) or clearing(s), no trusted maintainer to wake: %s", len(all), cut(strings.Join(all, "; "), 300))
		return nil
	}
	line := func(t string) string {
		return cut(strings.Join(strings.Fields(t), " "), 400)
	}
	form := func(k string) string {
		name := "ebac-" + critic + "-" + k
		if id := s.children[k]; id != "" {
			name += " (" + id + ")"
		}
		return name
	}
	if n := len(s.raised) + len(s.cleared); n > MaxAlarmPings {
		var ks []string
		for _, r := range s.raised {
			k, alarm, _ := strings.Cut(r, " ")
			alarm, _, _ = strings.Cut(alarm, ":")
			ks = append(ks, form(k)+" "+alarm)
		}
		for _, r := range s.cleared {
			k, alarm, _ := strings.Cut(r, " ")
			alarm, _, _ = strings.Cut(alarm, ":")
			ks = append(ks, form(k)+" "+alarm+" cleared")
		}
		return []plugin.Ping{{To: s.p.Maintainer, Text: line(fmt.Sprintf("ghcache: %d sync alarm change(s) this round (collect mode): %s", n, strings.Join(ks, "; ")))}}
	}
	var out []plugin.Ping
	for _, r := range s.raised {
		k, rest, _ := strings.Cut(r, " ")
		out = append(out, plugin.Ping{To: s.p.Maintainer, Text: line(fmt.Sprintf("ghcache alarm on %s: sync.alarm.%s", form(k), rest))})
	}
	for _, r := range s.cleared {
		k, rest, _ := strings.Cut(r, " ")
		out = append(out, plugin.Ping{To: s.p.Maintainer, Text: line(fmt.Sprintf("ghcache alarm cleared on %s: sync.alarm.%s", form(k), rest))})
	}
	return out
}

// syncKeyAlarms nests e.Alarms as sync.alarm.<kind>.<field>.
func syncKeyAlarms(e *Entity) map[string]map[string]Alarm {
	if len(e.Alarms) == 0 {
		return nil
	}
	out := map[string]map[string]Alarm{}
	for key, a := range e.Alarms {
		kind, field, _ := strings.Cut(key, ".")
		if out[kind] == nil {
			out[kind] = map[string]Alarm{}
		}
		out[kind][field] = a
	}
	return out
}
