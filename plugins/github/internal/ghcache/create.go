package ghcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	praxisstd "github.com/gluck/ebac/internal/praxis"
	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// Creation (A2): the two writes with no base to diff, so each is made
// idempotent instead.
//
// An ISSUE is asked for on the register (the critic's role form):
// ours.praxis_new.<id> = the praxis fields of the new issue, written by the
// issue CLI only. The sync validates and renders it (internal/praxis), and
// reports on the register's sync.praxis_new.<id>: "creating" is recorded and
// must land (one round) BEFORE the issue is created, so a create whose answer
// was lost is found again: the round after, our open issues in the repo are
// listed and the one whose body carries <!-- praxis-id: <id> --> is adopted,
// never created twice. Then the issue's entity form is minted holding the
// praxis keys, and sync.praxis_new.<id> = created {issue, form, at}.
//
// A COMMENT is asked for on an entity form: want.comments.new.<key> = {body}.
// Same rule: sync.comment_new.<key> = creating lands first; the comment body
// starts with <!-- k:<16 hex of sha256(form:key)> -->; a retry adopts the
// subject's comment carrying it.
//
// Every created text passes the disclose list first; a refusal is an alarm
// and nothing is written to GitHub.

// CreatedTTL is how long a created issue's sync.praxis_new record stays.
const CreatedTTL = 24 * time.Hour

// AdoptFirst is the page size of an adopt listing (our issues, a subject's
// comments).
const AdoptFirst = 50

// Create is one asked-for issue's bookkeeping (State.Creates, by praxis id).
type Create struct {
	Repo   string                     `json:"repo"`
	State  string                     `json:"state"` // creating | created | refused
	At     time.Time                  `json:"at"`
	Req    string                     `json:"req"`              // hash of the request it was recorded for
	Title  string                     `json:"title,omitempty"`  // as rendered when recorded: what is created
	Body   string                     `json:"body,omitempty"`   // ends with the praxis-id marker
	Praxis json.RawMessage            `json:"praxis,omitempty"` // the request, minted on the form
	Issue  int                        `json:"issue,omitempty"`
	URL    string                     `json:"url,omitempty"`
	Key    string                     `json:"key,omitempty"` // the minted child key
	Tries  int                        `json:"tries,omitempty"`
	Mint   map[string]json.RawMessage `json:"mint,omitempty"` // the minting write's keys, written again until the form is held
	Rule   string                     `json:"rule,omitempty"` // refused: the praxis rule or write-refused
	Detail string                     `json:"detail,omitempty"`
}

// CommentNew is one asked-for comment's bookkeeping (State.Comments, by
// "<child key>/<client key>").
type CommentNew struct {
	State string    `json:"state"` // creating | created | refused
	At    time.Time `json:"at"`
	Ask   string    `json:"ask,omitempty"`  // the body as asked (compared with the form's ask)
	Body  string    `json:"body,omitempty"` // as posted: the marker line, then the asked body
	ID    string    `json:"id,omitempty"`
	URL   string    `json:"url,omitempty"`
	Tries int       `json:"tries,omitempty"`
	Why   string    `json:"why,omitempty"`
}

func hashJSON(raw json.RawMessage) string { return hash12(string(raw)) }

// creator runs one round's creations; it shares the syncer's write budget.
type creator struct {
	s     *syncer
	st    *State
	p     Pass
	reg   map[string]any // the register's sync key, rebuilt every round
	mints int            // forms this round mints (they count against the form cap)
}

// regAlarm sets sync.alarm.<kind>.<field> on the register.
func (c *creator) regAlarm(kind, field string, v any) {
	al, _ := c.reg["alarm"].(map[string]any)
	if al == nil {
		al = map[string]any{}
		c.reg["alarm"] = al
	}
	k, _ := al[kind].(map[string]any)
	if k == nil {
		k = map[string]any{}
		al[kind] = k
	}
	k[field] = v
}

func (c *creator) regSet(group, key string, v any) {
	g, _ := c.reg[group].(map[string]any)
	if g == nil {
		g = map[string]any{}
		c.reg[group] = g
	}
	g[key] = v
}

// ourIssue is one of our issues as the adopt listing reads it.
type ourIssue struct {
	Number    int       `json:"number"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// pageInfo is a connection's paging.
type pageInfo struct {
	HasNext bool   `json:"hasNextPage"`
	HasPrev bool   `json:"hasPreviousPage"`
	End     string `json:"endCursor"`
	Start   string `json:"startCursor"`
}

// AdoptPages bounds an adopt listing (AdoptFirst each): a listing that has
// not reached items older than the record by then is inconclusive, and
// nothing is created.
const AdoptPages = 10

// adoptSlack: an item created up to this long before its record is still
// read (clock skew between us and GitHub).
const adoptSlack = 5 * time.Minute

// IssuesQuery lists our issues in repo, open AND closed, newest first, from
// cursor after ("" = the first page); and, when assignee is set, that
// user's node id.
func IssuesQuery(repo, login, assignee, after string) string {
	owner, name, _ := strings.Cut(repo, "/")
	cur := ""
	if after != "" {
		cur = fmt.Sprintf(",after:%q", after)
	}
	q := fmt.Sprintf(`{ repo: repository(owner:%q,name:%q){ id issues(first:%d%s,filterBy:{createdBy:%q},orderBy:{field:CREATED_AT,direction:DESC}){ nodes{ number url body createdAt } pageInfo{ hasNextPage endCursor } } }`,
		owner, name, AdoptFirst, cur, login)
	if assignee != "" {
		q += fmt.Sprintf(` assignee: user(login:%q){ id }`, assignee)
	}
	return q + " }"
}

// createIssueMutation creates an issue; assignee, when set, is a node id.
func createIssueMutation(assignee string) string {
	a := ""
	if assignee != "" {
		a = fmt.Sprintf(",assigneeIds:[%q]", assignee)
	}
	return `mutation($r:ID!,$t:String!,$b:String!){ createIssue(input:{repositoryId:$r,title:$t,body:$b` + a + `}){ issue{ number url } } }`
}

// issues creates the asked-for issues; it returns the form writes (minted
// issue forms).
func (c *creator) issues(asks map[string]json.RawMessage) []plugin.FormWrite {
	var out []plugin.FormWrite
	now := c.p.Now.UTC()
	if c.st.Creates == nil {
		c.st.Creates = map[string]*Create{}
	}
	ids := map[string]bool{}
	for id := range asks {
		ids[id] = true
	}
	for id := range c.st.Creates {
		ids[id] = true
	}
	order := make([]string, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)
	for _, id := range order {
		raw, asked := asks[id]
		cr := c.st.Creates[id]
		if cr != nil && cr.State == "created" {
			if !asked && now.Sub(cr.At) > CreatedTTL {
				delete(c.st.Creates, id)
				continue
			}
			if _, held := c.p.Children[cr.Key]; !held && c.p.Children != nil && len(cr.Mint) > 0 {
				// The minting write did not land: write it again (it lands
				// only on a form still without gh and praxis).
				out = append(out, plugin.FormWrite{Key: cr.Key, Set: cr.Mint, ExpectAbsent: []string{"gh", "praxis"}})
			}
			c.regSet("praxis_new", id, map[string]any{"state": "created", "issue": cr.Issue, "form": c.p.Children[cr.Key], "at": cr.At.Format(time.RFC3339)})
			continue
		}
		if cr != nil && cr.State == "creating" {
			if fw, ok := c.finish(id, cr); ok {
				out = append(out, fw...)
			}
			continue
		}
		if !asked {
			delete(c.st.Creates, id) // withdrawn before it was recorded, or a refusal no longer asked
			continue
		}
		if cr != nil && cr.State == "refused" && cr.Req == hashJSON(raw) {
			c.regAlarm(alarmKindOf(cr.Rule), alarmFieldOf(cr.Rule, id), map[string]any{"rule": strings.TrimPrefix(cr.Rule, "praxis:"), "detail": cr.Detail, "at": cr.At.Format(time.RFC3339)})
			continue
		}
		cr = c.record(id, raw)
		c.st.Creates[id] = cr
		if cr.State == "refused" {
			c.regAlarm(alarmKindOf(cr.Rule), alarmFieldOf(cr.Rule, id), map[string]any{"rule": strings.TrimPrefix(cr.Rule, "praxis:"), "detail": cr.Detail, "at": cr.At.Format(time.RFC3339)})
			c.s.logf("praxis_new %s refused: %s: %s", id, cr.Rule, cr.Detail)
			continue
		}
		// Recorded: GitHub is written the round after this record landed.
		c.regSet("praxis_new", id, map[string]any{"state": "creating", "at": cr.At.Format(time.RFC3339)})
	}
	return out
}

// alarmKindOf: a praxis rule is sync.alarm.praxis.<id>; a disclose or scope
// refusal is sync.alarm.write-refused.praxis_new.<id>.
func alarmKindOf(rule string) string {
	if strings.HasPrefix(rule, "praxis:") {
		return "praxis"
	}
	return AlarmWriteRefused
}

func alarmFieldOf(rule, id string) string {
	if strings.HasPrefix(rule, "praxis:") {
		return id
	}
	return "praxis_new." + id
}

// record validates and renders one request: a refused Create, or a
// creating one carrying exactly what will be created.
func (c *creator) record(id string, raw json.RawMessage) *Create {
	now := c.p.Now.UTC()
	cr := &Create{At: now, Req: hashJSON(raw), State: "refused"}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		cr.Rule, cr.Detail = AlarmUnparseable, err.Error()
		return cr
	}
	repo, _ := m["repo"].(string)
	cr.Repo = repo
	issue, err := praxisstd.FromForm(map[string]any{"praxis": m})
	if err != nil {
		cr.Rule, cr.Detail = AlarmUnparseable, err.Error()
		return cr
	}
	issue.ID, issue.Number = id, 0
	if why := c.p.Auth(model.GHEntity{Repo: repo, Kind: model.KindNewIssue}, model.FieldIssueCreate); why != "" {
		cr.Rule, cr.Detail = "scope", why
		return cr
	}
	r, err := praxisstd.Render(issue, c.parent(repo, issue.Parent))
	if err != nil {
		if ref, ok := err.(*praxisstd.Refusal); ok {
			cr.Rule, cr.Detail = "praxis:"+string(ref.Rule), ref.Detail
		} else {
			cr.Rule, cr.Detail = AlarmUnparseable, err.Error()
		}
		return cr
	}
	for _, t := range []string{r.Title, r.Body} {
		if why := c.p.discloses(t); why != "" {
			cr.Rule, cr.Detail = "disclose", why
			return cr
		}
	}
	cr.State, cr.Title, cr.Body, cr.Praxis = "creating", r.Title, r.Body, raw
	return cr
}

// parent is the state of issue #n of repo as its entity form shows it, nil
// when it has no form this round (the validator then checks the form only).
func (c *creator) parent(repo string, n int) *praxisstd.ParentState {
	if n == 0 {
		return nil
	}
	owned, ok := c.p.Owned[IssueKey(repo, n)]
	if !ok {
		return nil
	}
	var g struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(owned["gh"], &g)
	var px map[string]any
	_ = json.Unmarshal(owned["praxis"], &px)
	ps := &praxisstd.ParentState{Number: n, Open: g.State == "" || g.State == "OPEN"}
	if i, err := praxisstd.FromForm(map[string]any{"praxis": px}); err == nil {
		ps.Status = i.Status
	}
	return ps
}

// finish creates (or adopts) a recorded issue and mints its form. Every
// guard runs again right before the POST: the trusted scope and the
// disclose list may have changed since the record, and a held form count at
// the cap would make the mint refuse the whole reply.
func (c *creator) finish(id string, cr *Create) ([]plugin.FormWrite, bool) {
	keep := func() {
		c.regSet("praxis_new", id, map[string]any{"state": "creating", "at": cr.At.Format(time.RFC3339)})
	}
	refuse := func(rule, detail string) {
		cr.State, cr.Rule, cr.Detail = "refused", rule, detail
		c.regAlarm(alarmKindOf(rule), alarmFieldOf(rule, id), map[string]any{"rule": strings.TrimPrefix(rule, "praxis:"), "detail": detail, "at": c.p.Now.UTC().Format(time.RFC3339)})
	}
	var px map[string]any
	_ = json.Unmarshal(cr.Praxis, &px)
	assignee, _ := px["assignee"].(string)

	// Adopt: our issues, open and closed, newest first, until one older than
	// the record (then every issue a lost create could have made was read).
	marker := praxisstd.Marker(id)
	since := cr.At.Add(-adoptSlack)
	var repoID, assigneeID, after string
	conclusive := false
	for page := 0; page < AdoptPages; page++ {
		data, errs, err := c.s.gh.GraphQLPartial(c.s.ctx, IssuesQuery(cr.Repo, c.p.Ours, assignee, after))
		if err != nil || len(errs) > 0 {
			// A partial answer is no proof the issue is absent: nothing is created.
			c.s.logf("praxis_new %s: listing our issues failed (retried next round): %v %v", id, err, errs)
			keep()
			return nil, false
		}
		var ans struct {
			Repo *struct {
				ID     string `json:"id"`
				Issues *struct {
					Nodes    []ourIssue `json:"nodes"`
					PageInfo pageInfo   `json:"pageInfo"`
				} `json:"issues"`
			} `json:"repo"`
			Assignee *struct {
				ID string `json:"id"`
			} `json:"assignee"`
		}
		if json.Unmarshal(data, &ans) != nil || ans.Repo == nil || ans.Repo.ID == "" || ans.Repo.Issues == nil {
			c.s.logf("praxis_new %s: listing our issues unreadable (retried next round)", id)
			keep()
			return nil, false
		}
		repoID = ans.Repo.ID
		if ans.Assignee != nil {
			assigneeID = ans.Assignee.ID
		}
		for _, is := range ans.Repo.Issues.Nodes {
			if strings.Contains(is.Body, marker) {
				c.s.logf("praxis_new %s: adopted #%d (its create's answer was lost)", id, is.Number)
				return c.created(id, cr, is.Number, is.URL), true
			}
			if is.CreatedAt.Before(since) {
				conclusive = true
			}
		}
		if conclusive || !ans.Repo.Issues.PageInfo.HasNext {
			conclusive = true
			break
		}
		after = ans.Repo.Issues.PageInfo.End
	}
	if !conclusive {
		c.s.logf("praxis_new %s: %d pages of our issues are all newer than its record: not created (retried next round)", id, AdoptPages)
		keep()
		return nil, false
	}

	if why := c.p.Auth(model.GHEntity{Repo: cr.Repo, Kind: model.KindNewIssue}, model.FieldIssueCreate); why != "" {
		refuse("scope", why)
		return nil, false
	}
	for _, t := range []string{cr.Title, cr.Body} {
		if why := c.p.discloses(t); why != "" {
			refuse("disclose", why)
			return nil, false
		}
	}
	if max := maxForms(c.p.Config); len(c.p.Children)+c.mints >= max {
		c.s.logf("praxis_new %s: the cache holds %d forms (max %d): created when one is free", id, len(c.p.Children)+c.mints, max)
		keep()
		return nil, false
	}
	if c.s.writes >= MaxWrites {
		keep()
		return nil, false
	}
	c.s.cur = cr.Repo + "#new"
	resp, err := c.s.mutate(createIssueMutation(assigneeID), map[string]string{"r": repoID, "t": cr.Title, "b": cr.Body})
	if err != nil {
		cr.Tries++
		if refusedByGitHub(err) || cr.Tries >= MaxTries {
			refuse("github", cut(err.Error(), 300))
			return nil, false
		}
		keep()
		return nil, false
	}
	var made struct {
		CreateIssue struct {
			Issue struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			} `json:"issue"`
		} `json:"createIssue"`
	}
	if json.Unmarshal(resp, &made) != nil || made.CreateIssue.Issue.Number == 0 {
		// Created but unreadable: the next round's listing adopts it.
		keep()
		return nil, false
	}
	return c.created(id, cr, made.CreateIssue.Issue.Number, made.CreateIssue.Issue.URL), true
}

// discloses is the disclose list's verdict on a text GitHub would show
// (Pass.Disclose in tests, else the sdk's list).
func (p Pass) discloses(t string) string {
	if p.Disclose != nil {
		return p.Disclose(t)
	}
	return disclose.Disclosure(t)
}

// maxForms is the forms the cache may hold (Config.Max, else DefaultMax).
func maxForms(cfg *Config) int {
	if cfg != nil && cfg.Max > 0 {
		return cfg.Max
	}
	return DefaultMax
}

// created records issue n as id's, enrols it as a tracked entity (ours:
// kept without a rule) and mints its entity form.
func (c *creator) created(id string, cr *Create, n int, url string) []plugin.FormWrite {
	now := c.p.Now.UTC()
	cr.State, cr.Issue, cr.URL, cr.Key, cr.At = "created", n, url, IssueKey(cr.Repo, n), now
	var px map[string]any
	_ = json.Unmarshal(cr.Praxis, &px)
	delete(px, "repo")
	px["number"], px["id"] = n, id
	gh := map[string]any{"kind": "issue", "number": n, "url": url, "state": "OPEN", "title": cr.Title, "author": c.p.Ours}
	c.regSet("praxis_new", id, map[string]any{"state": "created", "issue": n, "form": c.p.Children[cr.Key], "at": cr.At.Format(time.RFC3339)})
	if p, _ := px["parent"].(float64); p != 0 {
		c.regSet("praxis_children_add", fmt.Sprint(int(p)), n)
	}
	if c.st.Entities == nil {
		c.st.Entities = map[string]*Entity{}
	}
	if c.st.Entities[cr.Key] == nil {
		c.st.Entities[cr.Key] = &Entity{Kind: KindIssue, Repo: cr.Repo, N: n, Tier: "hot", Since: now, Seen: now}
	}
	if c.st.OurIssues == nil {
		c.st.OurIssues = map[string]Ref{}
	}
	c.st.OurIssues[cr.Key] = Ref{Repo: cr.Repo, N: n}
	c.mints++
	cr.Mint = map[string]json.RawMessage{"gh": mustJSON(gh), "praxis": mustJSON(px)}
	return []plugin.FormWrite{{Key: cr.Key, Set: cr.Mint, ExpectAbsent: []string{"gh", "praxis"}}}
}

// wantView is an entity form's want key as the core read it.
type wantView struct {
	raw json.RawMessage
	New map[string]struct {
		Body string `json:"body"`
	} // want.comments.new
}

func readWant(raw json.RawMessage) *wantView {
	w := &wantView{raw: raw}
	var v struct {
		Comments struct {
			New map[string]struct {
				Body string `json:"body"`
			} `json:"new"`
		} `json:"comments"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
		w.New = v.Comments.New
	}
	return w
}

// CommentsQuery reads a subject's comments with their bodies, newest first
// (backwards from cursor before; "" = the newest page), for the adopt check
// of a comment create.
func CommentsQuery(node, before string) string {
	cur := ""
	if before != "" {
		cur = fmt.Sprintf(",before:%q", before)
	}
	sel := fmt.Sprintf(`comments(last:%d%s){ nodes{ id url body createdAt } pageInfo{ hasPreviousPage startCursor } }`, AdoptFirst, cur)
	return fmt.Sprintf(`{ subj: node(id:%q){ ... on Issue{ %s } ... on PullRequest{ %s } } }`, node, sel, sel)
}

const addCommentMutation = `mutation($s:ID!,$b:String!){ addComment(input:{subjectId:$s,body:$b}){ commentEdge{ node{ id url } } } }`

// CommentMarker is the hidden first line of a created comment: a hash of the
// form, the client key and the asked body, so no tool name and no form id
// reach GitHub, and a client key reused with new text is a new comment.
func CommentMarker(form, key, body string) string {
	h := sha256.Sum256([]byte(form + ":" + key + ":" + body))
	return "<!-- k:" + hex.EncodeToString(h[:])[:16] + " -->"
}

// postedComment is one comment as the adopt listing reads it.
type postedComment struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// findComment looks for the comment whose first line is marker among the
// subject's comments newer than since. ok false: the listing failed or was
// inconclusive (no proof it is absent).
func (s *syncer) findComment(node, marker string, since time.Time) (found *postedComment, ok bool) {
	before := ""
	for page := 0; page < AdoptPages; page++ {
		data, errs, err := s.gh.GraphQLPartial(s.ctx, CommentsQuery(node, before))
		if err != nil || len(errs) > 0 {
			return nil, false
		}
		var ans struct {
			Subj *struct {
				Comments *struct {
					Nodes    []postedComment `json:"nodes"`
					PageInfo pageInfo        `json:"pageInfo"`
				} `json:"comments"`
			} `json:"subj"`
		}
		if json.Unmarshal(data, &ans) != nil || ans.Subj == nil || ans.Subj.Comments == nil {
			return nil, false
		}
		older := false
		for i := range ans.Subj.Comments.Nodes {
			c := &ans.Subj.Comments.Nodes[i]
			if first, _, _ := strings.Cut(c.Body, "\n"); strings.TrimRight(first, "\r") == marker {
				return c, true
			}
			if c.CreatedAt.Before(since) {
				older = true
			}
		}
		if older || !ans.Subj.Comments.PageInfo.HasPrev {
			return nil, true
		}
		before = ans.Subj.Comments.PageInfo.Start
	}
	return nil, false
}

// comments runs entity k's comment creates (want.comments.new.<key>) against
// subject node. It returns the want key to write (nil: unchanged) and the
// sync.comment_new records to show.
func (s *syncer) comments(k string, e *Entity, ent model.GHEntity, node string, w *wantView) (json.RawMessage, map[string]any) {
	if e.CommentNew == nil {
		e.CommentNew = map[string]*CommentNew{}
	}
	shown := map[string]any{}
	done := map[string]bool{}
	keys := make([]string, 0, len(w.New))
	for key := range w.New {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	form := s.children[k]
	if form == "" {
		form = k
	}
	guard := func(body string) string {
		if why := s.p.Auth(ent, model.FieldCommentCreate); why != "" {
			return why
		}
		if strings.TrimSpace(body) == "" {
			return "empty body"
		}
		return s.p.discloses(body)
	}
	record := func(key, body string) *CommentNew {
		field := "comments.new." + key
		cn := &CommentNew{At: s.now, Ask: body}
		if why := guard(body); why != "" {
			cn.State, cn.Why = "refused", why
			s.alarm(k, e, AlarmWriteRefused, field, why, nil, nil, nil)
		} else {
			// Recorded: posted the round after this record landed.
			cn.State, cn.Body = "creating", CommentMarker(form, key, body)+"\n"+body
		}
		e.CommentNew[key] = cn
		return cn
	}
	for _, key := range keys {
		field := "comments.new." + key
		body := w.New[key].Body
		cn := e.CommentNew[key]
		switch {
		case cn == nil:
			cn = record(key, body)
		case cn.State == "refused" && cn.Ask != body:
			cn = record(key, body) // a refused ask, changed: judged again
		case cn.State == "created" && cn.Ask != body:
			cn = record(key, body) // the key asked again with new text: a new comment
		case cn.State == "creating":
			marker, _, _ := strings.Cut(cn.Body, "\n")
			got, ok := s.findComment(node, marker, cn.At.Add(-adoptSlack))
			if !ok {
				s.logf("%s: %s: reading the subject's comments failed or was inconclusive (retried next round)", k, field)
				break
			}
			if got != nil {
				cn.State, cn.ID, cn.URL = "created", got.ID, got.URL
				s.logf("%s: %s adopted %s (its create's answer was lost)", k, field, got.ID)
				if cn.Ask != body {
					cn = record(key, body) // the old text is on GitHub; the new ask is its own comment
				}
				break
			}
			if cn.Ask != body {
				cn = record(key, body) // never posted: the new text replaces it
				break
			}
			if why := guard(body); why != "" {
				cn.State, cn.Why = "refused", why
				s.alarm(k, e, AlarmWriteRefused, field, why, nil, nil, nil)
				break
			}
			if s.writes >= MaxWrites {
				break
			}
			s.cur = k
			resp, err := s.mutate(addCommentMutation, map[string]string{"s": node, "b": cn.Body})
			if err != nil {
				cn.Tries++
				if refusedByGitHub(err) || cn.Tries >= MaxTries {
					cn.State, cn.Why = "refused", cut(err.Error(), 300)
					s.alarm(k, e, AlarmWriteRefused, field, cn.Why, nil, nil, nil)
				}
				break
			}
			var made struct {
				AddComment struct {
					CommentEdge struct {
						Node struct {
							ID  string `json:"id"`
							URL string `json:"url"`
						} `json:"node"`
					} `json:"commentEdge"`
				} `json:"addComment"`
			}
			if json.Unmarshal(resp, &made) == nil && made.AddComment.CommentEdge.Node.ID != "" {
				cn.State, cn.ID, cn.URL = "created", made.AddComment.CommentEdge.Node.ID, made.AddComment.CommentEdge.Node.URL
			} // else: posted, answer unreadable: the next round's listing adopts it
		}
		rec := map[string]any{"state": cn.State, "at": cn.At.Format(time.RFC3339)}
		if cn.ID != "" {
			rec["id"], rec["url"] = cn.ID, cn.URL
		}
		if cn.Why != "" {
			rec["why"] = cut(cn.Why, 200)
		}
		shown[key] = rec
		if cn.State == "created" && cn.Ask == body {
			done[key] = true // the ask as read is the one posted: it leaves the form
		}
	}
	for key, cn := range e.CommentNew {
		if _, asked := w.New[key]; !asked && (cn.State != "creating" || s.now.Sub(cn.At) > CreatedTTL) {
			delete(e.CommentNew, key) // the seat removed the ask (or it was done and removed)
		}
	}
	if len(done) == 0 {
		return nil, shown
	}
	// Done asks leave the want key; the rest of it is kept as read. The write
	// lands only if want is still as read (Expect): an ask edited meanwhile
	// stays, and its new text is judged next round.
	var all map[string]any
	_ = json.Unmarshal(w.raw, &all)
	if cs, _ := all["comments"].(map[string]any); cs != nil {
		if nw, _ := cs["new"].(map[string]any); nw != nil {
			for key := range done {
				delete(nw, key)
			}
			if len(nw) == 0 {
				delete(cs, "new")
			}
		}
		if len(cs) == 0 {
			delete(all, "comments")
		}
	}
	return mustJSON(all), shown
}
