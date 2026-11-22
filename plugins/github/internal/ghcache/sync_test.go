package ghcache

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// Every fixture here is synthesized: repo acme/widget, logins our-author / other-author / stranger, node ids
// PR_<n>, PRRT_<n>_<i>, LA_<hex name>. No captured PR, login, body or id.

// fakeWrites is the GitHub side of the write path: the labels the repo has, what the operator posted per thread,
// every mutation, and hooks to move GitHub between the deep read and the pre-write read, or fail a mutation.
type fakeWrites struct {
	repoLabels  map[string]bool
	mine        map[string][]string // thread id -> bodies the operator posted
	mutations   []string
	beforeFresh func()
	failMutate  func(q string) error
	replyState  string // "" is SUBMITTED
	// staleDeep, by PR number, is what the deep read answers instead of GitHub's current PR (a lagging cache);
	// the pre-write read always sees GitHub's current state.
	staleDeep map[int]*PR
}

var (
	preNodeRe    = regexp.MustCompile(`pre: node\(id:"([^"]+)"\)`)
	preThreadRe  = regexp.MustCompile(`(t\d+): node\(id:"([^"]+)"\)`)
	preLabelRe   = regexp.MustCompile(`(l\d+): label\(name:("(?:[^"\\]|\\.)*")\)`)
	mutPRRe      = regexp.MustCompile(`(?:pullRequestId|labelableId):"([^"]+)"`)
	mutLabelsRe  = regexp.MustCompile(`labelIds:\[([^\]]*)\]`)
	mutThreadRe  = regexp.MustCompile(`threadId:"([^"]+)"`)
	updatedAtFmt = "2006-01-02T15:04:05Z"
)

func (f *fakeGH) findPR(id string) *PR {
	for _, byN := range f.prs {
		for _, pr := range byN {
			if pr.ID == id {
				return pr
			}
		}
	}
	return nil
}

func (f *fakeGH) findThread(id string) (*PR, int) {
	for _, byN := range f.prs {
		for _, pr := range byN {
			for i, t := range pr.Threads.Nodes {
				if t.ID == id {
					return pr, i
				}
			}
		}
	}
	return nil, -1
}

func bump(pr *PR) {
	at, _ := time.Parse(updatedAtFmt, pr.UpdatedAt)
	pr.UpdatedAt = at.Add(time.Second).Format(updatedAtFmt)
}

// preWrite answers the write path's re-read as GitHub does: the PR node's draft and labels, each thread node's
// resolution and newest comments (viewerDidAuthor for the operator's), and each asked label (a missing one is null
// plus a NOT_FOUND on [repo, lN]).
func (f *fakeGH) preWrite(q string) (json.RawMessage, []forge.GQLError, error) {
	if f.wr == nil {
		f.t.Fatalf("a pre-write read with no write path expected: %s", q)
	}
	if f.wr.beforeFresh != nil {
		f.wr.beforeFresh()
		f.wr.beforeFresh = nil
	}
	out := map[string]any{}
	var errs []forge.GQLError
	m := preNodeRe.FindStringSubmatch(q)
	if pr := f.findPR(m[1]); pr != nil {
		var ls []any
		for _, l := range pr.Labels.Nodes {
			ls = append(ls, map[string]any{"id": labelID(l.Name), "name": l.Name})
		}
		out["pre"] = map[string]any{"isDraft": pr.Draft, "labels": map[string]any{"totalCount": pr.Labels.TotalCount, "nodes": ls}}
	} else {
		out["pre"] = nil
	}
	for _, tm := range preThreadRe.FindAllStringSubmatch(q, -1) {
		pr, i := f.findThread(tm[2])
		if pr == nil {
			out[tm[1]] = nil
			continue
		}
		var cs []any
		for _, b := range f.wr.mine[tm[2]] {
			cs = append(cs, map[string]any{"body": b, "viewerDidAuthor": true})
		}
		out[tm[1]] = map[string]any{"isResolved": pr.Threads.Nodes[i].Resolved, "comments": map[string]any{"nodes": cs}}
	}
	if strings.Contains(q, " repo: repository(") {
		repo := map[string]any{}
		for _, lm := range preLabelRe.FindAllStringSubmatch(q, -1) {
			var name string
			_ = json.Unmarshal([]byte(lm[2]), &name)
			if f.wr.repoLabels[name] {
				repo[lm[1]] = map[string]any{"id": labelID(name), "name": name}
			} else {
				repo[lm[1]] = nil
				errs = append(errs, forge.GQLError{Type: "NOT_FOUND", Path: []string{"repo", lm[1]}, Message: "Could not resolve to a Label with the name of '" + name + "'."})
			}
		}
		out["repo"] = repo
	}
	data, err := json.Marshal(out)
	return data, errs, err
}

// Mutate applies a write to the fake world as GitHub would, and records it.
func (f *fakeGH) Mutate(_ context.Context, q string, vars map[string]string) (json.RawMessage, error) {
	if f.wr == nil {
		f.t.Fatalf("a mutation with no write path expected: %s", q)
	}
	rec := q
	if vars["b"] != "" {
		rec += " b=" + vars["b"]
	}
	f.wr.mutations = append(f.wr.mutations, rec)
	if f.wr.failMutate != nil {
		if err := f.wr.failMutate(q); err != nil {
			return nil, err
		}
	}
	labelNames := func() []string {
		var out []string
		if m := mutLabelsRe.FindStringSubmatch(q); m != nil {
			for _, id := range strings.Split(m[1], ",") {
				b, err := hex.DecodeString(strings.TrimPrefix(strings.Trim(id, `"`), "LA_"))
				if err != nil {
					f.t.Fatalf("label id %s is not one the fake gave out", id)
				}
				out = append(out, string(b))
			}
		}
		return out
	}
	switch {
	case strings.Contains(q, "convertPullRequestToDraft"), strings.Contains(q, "markPullRequestReadyForReview"):
		pr := f.findPR(mutPRRe.FindStringSubmatch(q)[1])
		pr.Draft = strings.Contains(q, "convertPullRequestToDraft")
		bump(pr)
		return json.RawMessage(`{"x":{"pullRequest":{"isDraft":true}}}`), nil
	case strings.Contains(q, "addLabelsToLabelable"):
		pr := f.findPR(mutPRRe.FindStringSubmatch(q)[1])
		for _, n := range labelNames() {
			pr.Labels.Nodes = append(pr.Labels.Nodes, struct {
				Name string `json:"name"`
			}{n})
		}
		pr.Labels.TotalCount = len(pr.Labels.Nodes)
		bump(pr)
		return json.RawMessage(`{"addLabelsToLabelable":{"clientMutationId":null}}`), nil
	case strings.Contains(q, "removeLabelsFromLabelable"):
		pr := f.findPR(mutPRRe.FindStringSubmatch(q)[1])
		rm := labelNames()
		pr.Labels.Nodes = slices.DeleteFunc(pr.Labels.Nodes, func(l struct {
			Name string `json:"name"`
		}) bool {
			return slices.Contains(rm, l.Name)
		})
		pr.Labels.TotalCount = len(pr.Labels.Nodes)
		bump(pr)
		return json.RawMessage(`{"removeLabelsFromLabelable":{"clientMutationId":null}}`), nil
	case strings.Contains(q, "resolveReviewThread"):
		pr, i := f.findThread(mutThreadRe.FindStringSubmatch(q)[1])
		pr.Threads.Nodes[i].Resolved = true
		bump(pr)
		return json.RawMessage(`{"resolveReviewThread":{"thread":{"isResolved":true}}}`), nil
	case strings.Contains(q, "addPullRequestReviewThreadReply"):
		pr, i := f.findThread(vars["t"])
		if f.wr.mine == nil {
			f.wr.mine = map[string][]string{}
		}
		f.wr.mine[vars["t"]] = append(f.wr.mine[vars["t"]], vars["b"])
		th := &pr.Threads.Nodes[i]
		th.Comments.TotalCount++
		th.Comments.Nodes = []struct {
			Author *Login `json:"author"`
		}{{Author: &Login{Login: "operator"}}}
		bump(pr)
		state := model.FirstNonEmpty(f.wr.replyState, "SUBMITTED")
		return json.RawMessage(`{"addPullRequestReviewThreadReply":{"comment":{"state":"` + state + `","url":"https://example.invalid/pull/1#r1"}}}`), nil
	}
	f.t.Fatalf("unknown mutation %s", q)
	return nil, nil
}

// labelID is the fake's node id of a label: LA_ and the name in hex (a node id has no ':' or space; names do).
func labelID(name string) string { return "LA_" + hex.EncodeToString([]byte(name)) }

// synthPR is a production-shaped deep read (the JSON GitHub returns for deepSel) of acme/widget#n.
func synthPR(t *testing.T, n int, author string, labels []string, draft bool, threads int) *PR {
	t.Helper()
	var ls, ts []any
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	for i := 1; i <= threads; i++ {
		ts = append(ts, map[string]any{"id": fmt.Sprintf("PRRT_%d_%d", n, i), "path": fmt.Sprintf("src/f%d.go", i), "isResolved": false,
			"comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"author": map[string]any{"login": "reviewer"}}}}})
	}
	doc := map[string]any{"id": fmt.Sprintf("PR_%d", n), "number": n, "url": fmt.Sprintf("https://example.invalid/acme/widget/pull/%d", n),
		"title": fmt.Sprintf("feat: change %d", n), "body": "## Summary\nA synthesized change.\n", "state": "OPEN", "isDraft": draft,
		"mergeable": "MERGEABLE", "mergeStateStatus": "BLOCKED", "headRefOid": fmt.Sprintf("%040d", n), "headRefName": fmt.Sprintf("work/%d", n),
		"baseRefName": "main", "additions": 3, "deletions": 1, "updatedAt": "2026-11-20T04:00:00Z", "createdAt": "2026-11-19T04:00:00Z",
		"author": map[string]any{"login": author}, "labels": map[string]any{"totalCount": len(ls), "nodes": ls},
		"reviewThreads": map[string]any{"totalCount": len(ts), "nodes": ts}}
	var pr PR
	if err := json.Unmarshal(mustJSON(doc), &pr); err != nil {
		t.Fatal(err)
	}
	return &pr
}

const trustedMaint = "@0000000a"

// writeWorld: acme/widget#7 (ours) and #8 (other-author, read-only), each with two threads; both tracked by one
// explicit-PR rule; the critic's trusted scope writes ours only.
type writeWorld struct {
	t       *testing.T
	gh      *fakeGH
	c       *Config
	ttl     time.Duration
	st      State
	forms   map[string]map[string]json.RawMessage // child key -> the form's keys, as the core holds them
	critic  *model.Critic
	now     time.Time
	r       Result
	moveAt  string // a child key whose form a seat edits between the core's read and its write (Expect fails)
	moveKey string // which key that edit touches: gh (a read-only field) or sync
	moveFn  func() // when set, the seat's edit at moveAt (instead of moveKey's)
}

func newWriteWorld(t *testing.T, config string) *writeWorld {
	gh := &fakeGH{t: t, failLand: -1, children: map[string]string{}, search: map[string][]int{},
		prs: map[string]map[int]*PR{"acme/widget": {7: synthPR(t, 7, "our-author", []string{"area:core"}, false, 2), 8: synthPR(t, 8, "other-author", nil, false, 2),
			9: synthPR(t, 9, "stranger", nil, false, 0)}},
		wr: &fakeWrites{repoLabels: map[string]bool{"area:core": true, "needs-review": true, "blocked": true}}}
	c, ttl := cfg(t, config)
	w := &writeWorld{t: t, gh: gh, c: c, ttl: ttl, forms: map[string]map[string]json.RawMessage{}, now: t0,
		critic: &model.Critic{Name: "ghc", Source: model.SourceGHCache, Write: true, GHWrite: &model.GHWrite{
			Scope:  model.GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}, ReadOnlyAuthors: []string{"other-author"}},
			Fields: model.GHWriteFields}, Maintainer: trustedMaint}}
	w.round() // the first round mints the forms and agrees the base
	return w
}

const explicitPRs = `{"repos":["acme/widget"],"rules":[{"repo":"acme/widget","prs":[7,8]}]}`

// round is one core round: the core reads each child's gh and sync, runs the plugin, lands its writes in order
// (each only while its Expect holds; it stops at the first that does not), and keeps the state through JSON.
func (w *writeWorld) round() Result {
	w.t.Helper()
	owned := map[string]map[string]json.RawMessage{}
	for k := range w.gh.children {
		m := map[string]json.RawMessage{}
		for _, key := range model.GHCacheChildReadKeys {
			if v, ok := w.forms[k][key]; ok {
				m[key] = v
			}
		}
		owned[k] = m
	}
	kids := map[string]string{}
	for k, v := range w.gh.children {
		kids[k] = v
	}
	r, err := Run(context.Background(), w.gh, w.st, Pass{Config: w.c, ClosedTTL: w.ttl, Now: w.now, Children: kids, Landed: w.gh.landed,
		Owned: owned, Critic: w.critic.Name, Maintainer: w.critic.Maintainer,
		Auth: func(e model.GHEntity, field string) string { return model.AuthorizeGHWrite(w.critic, e, field) }})
	if err != nil {
		w.t.Fatalf("round at %s: %v", w.now, err)
	}
	w.gh.landed = 0
	for _, f := range r.Forms {
		if f.Key == w.moveAt {
			w.moveAt = ""
			if w.moveFn != nil {
				w.moveFn()
				w.moveFn = nil
			} else if w.moveKey == "gh" { // the form moved: Expect fails
				w.edit(f.Key, func(g map[string]any) { g["title"] = "retitled between the core's read and its write" })
			} else {
				w.editRaw(f.Key, "sync", json.RawMessage(`{"seen_by":"a seat, between the core's read and its write"}`))
			}
		}
		held := true
		for k, v := range f.Expect {
			if !sameJSON(w.forms[f.Key][k], v) {
				held = false
			}
		}
		for _, k := range f.ExpectAbsent {
			if _, ok := w.forms[f.Key][k]; ok {
				held = false
			}
		}
		if !held {
			break
		}
		if w.forms[f.Key] == nil {
			w.forms[f.Key] = map[string]json.RawMessage{}
		}
		for k, v := range f.Set {
			w.forms[f.Key][k] = v
		}
		w.gh.children[f.Key] = "@" + f.Key
		w.gh.landed++
	}
	var back State
	if err := json.Unmarshal(mustJSON(r.State), &back); err != nil {
		w.t.Fatal(err)
	}
	w.st, w.r = back, r
	w.now = w.now.Add(HotEvery)
	return r
}

// editRaw sets one top-level key of a form outside gh (a seat's unrelated edit).
func (w *writeWorld) editRaw(k, key string, v json.RawMessage) { w.forms[k][key] = v }

// edit changes the form's gh as a seat would (figaro set gh.<path>).
func (w *writeWorld) edit(k string, change func(g map[string]any)) {
	w.t.Helper()
	var g map[string]any
	if err := json.Unmarshal(w.forms[k]["gh"], &g); err != nil {
		w.t.Fatal(err)
	}
	change(g)
	w.forms[k]["gh"] = mustJSON(g)
}

// ack sets sync.alarm.<kind>.<field>.ack on a form.
func (w *writeWorld) ack(k, kind, field string) {
	w.t.Helper()
	var s map[string]any
	_ = json.Unmarshal(w.forms[k]["sync"], &s)
	s["alarm"].(map[string]any)[kind].(map[string]any)[field].(map[string]any)["ack"] = true
	w.forms[k]["sync"] = mustJSON(s)
}

func (w *writeWorld) formGH(k string) GH {
	w.t.Helper()
	var g GH
	if err := json.Unmarshal(w.forms[k]["gh"], &g); err != nil {
		w.t.Fatal(err)
	}
	return g
}

func (w *writeWorld) formSync(k string) SyncKey {
	w.t.Helper()
	var s SyncKey
	if err := json.Unmarshal(w.forms[k]["sync"], &s); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *writeWorld) mutations() []string {
	m := w.gh.wr.mutations
	w.gh.wr.mutations = nil
	return m
}

func addLabel(l string) func(map[string]any) {
	return func(g map[string]any) {
		ls, _ := g["labels"].([]any)
		g["labels"] = append(ls, l)
	}
}

func thread(g map[string]any, id string) map[string]any {
	return g["threads"].(map[string]any)[id].(map[string]any)
}

func pingTexts(r Result) []string {
	var out []string
	for _, p := range r.Pings {
		out = append(out, p.To+" "+p.Text)
	}
	return out
}

// C1: a write on a PR whose author is read-only in the trusted scope is refused: no mutation, the form's label is
// reverted to GitHub's, sync.alarm.write-refused.labels names why, and the maintainer is woken once. Control: the
// same edit on our PR in the same round is written. RED if the plugin stops authorizing (the refused row writes)
// or authorizes from the form (both or neither write).
func TestAWriteOnAReadOnlyAuthorsPRIsRefused(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("the first round wrote to GitHub: %v", m)
	}
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.edit("widget-pr-8", addLabel("needs-review"))
	r := w.round()
	if m := w.mutations(); !slices.Equal(m, []string{`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`}) {
		t.Fatalf("mutations %q", m)
	}
	if got := w.formGH("widget-pr-7").Labels; !slices.Equal(got, []string{"area:core", "needs-review"}) {
		t.Errorf("our PR's form labels %v", got)
	}
	if got := w.formGH("widget-pr-8").Labels; len(got) != 0 {
		t.Errorf("the refused form keeps the seat's labels %v (want GitHub's: none)", got)
	}
	a := w.formSync("widget-pr-8").Alarm[AlarmWriteRefused]["labels"]
	if a.Detail != "acme/widget#8: author other-author is read-only in the trusted scope" || a.At != "2026-11-20T04:05:00Z" {
		t.Errorf("alarm %+v", a)
	}
	if got := pingTexts(r); !slices.Equal(got, []string{trustedMaint + " ghcache alarm on ebac-ghc-widget-pr-8 (@widget-pr-8): sync.alarm.write-refused.labels: acme/widget#8: author other-author is read-only in the trusted scope"}) {
		t.Errorf("pings %q", got)
	}
	if _, ok := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]; ok {
		t.Errorf("our PR carries an alarm: %+v", w.formSync("widget-pr-7").Alarm)
	}
	// The seat tries again before anyone acks: refused again, the alarm stands, no second wake.
	w.edit("widget-pr-8", addLabel("needs-review"))
	r = w.round()
	if m := w.mutations(); len(m) != 0 || len(r.Pings) != 0 || w.formSync("widget-pr-8").Alarm[AlarmWriteRefused]["labels"].At != "2026-11-20T04:05:00Z" {
		t.Errorf("the repeat: mutations %q pings %q alarm %+v", m, pingTexts(r), w.formSync("widget-pr-8").Alarm)
	}
}

// C2: the register's config is seat-writable. A rule that tracks a stranger's PR and a config.scope naming it widen
// what is READ, never what is written: the edit is refused. Control: the trusted scope naming the PR (prs) lets the
// same edit through. RED if the scope is read from the config.
func TestAFormWidenedScopeHasNoEffect(t *testing.T) {
	widened := `{"repos":["acme/widget"],"rules":[{"repo":"acme/widget","prs":[7,9]}],"scope":{"repos":["acme/widget"],"prs":["acme/widget#9"]},"write_fields":["labels"],"maintainer":"@evil"}`
	w := newWriteWorld(t, widened)
	w.edit("widget-pr-9", addLabel("blocked"))
	r := w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("a form-widened scope wrote: %q", m)
	}
	if a := w.formSync("widget-pr-9").Alarm[AlarmWriteRefused]["labels"]; a.Detail != "acme/widget#9 (author stranger) is outside the trusted scope" {
		t.Errorf("alarm %+v", a)
	}
	if len(r.Pings) != 1 || r.Pings[0].To != trustedMaint {
		t.Errorf("pings %+v (want one, to the trusted maintainer, not the config's)", r.Pings)
	}
	// Control: the trusted config names the PR.
	c := newWriteWorld(t, widened)
	c.critic.GHWrite.Scope.PRs = []string{"acme/widget#9"}
	c.edit("widget-pr-9", addLabel("blocked"))
	c.round()
	if m := c.mutations(); !slices.Equal(m, []string{`mutation{addLabelsToLabelable(input:{labelableId:"PR_9",labelIds:["` + labelID("blocked") + `"]}){clientMutationId}}`}) {
		t.Fatalf("trusted scope: mutations %q", m)
	}
}

// C3: GitHub moves between the deep read and the write: the re-read sees it and the write is refused as stale (no
// mutation; the seat's wish stays on the form). The next round merges against GitHub's new value and writes. A
// draft stale twice is given up: write-refused alarm, GitHub's value stands, the form's in sync.conflicts. Control:
// no move, the same edit writes in its first round. RED if the re-read check goes (round one writes).
func TestAStaleBaseIsRefused(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	pr7 := w.gh.prs["acme/widget"][7]
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.gh.wr.beforeFresh = func() { // someone labels the PR on GitHub between our read and our write
		pr7.Labels.Nodes = append(pr7.Labels.Nodes, struct {
			Name string `json:"name"`
		}{"blocked"})
		pr7.Labels.TotalCount++
		bump(pr7)
	}
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("a stale write went out: %q", m)
	}
	if got := w.formGH("widget-pr-7").Labels; !slices.Equal(got, []string{"area:core", "needs-review"}) {
		t.Fatalf("the seat's wish is gone from the form: %v", got)
	}
	w.round()
	if m := w.mutations(); !slices.Equal(m, []string{`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`}) {
		t.Fatalf("after the stale round: %q", m)
	}
	if got := w.formGH("widget-pr-7").Labels; !slices.Equal(got, []string{"area:core", "blocked", "needs-review"}) {
		t.Fatalf("merged labels %v", got)
	}
	// Draft, stale twice in a row: given up.
	w.edit("widget-pr-7", func(g map[string]any) { g["draft"] = true })
	w.gh.wr.beforeFresh = func() { pr7.Draft = true; bump(pr7) } // GitHub goes to draft between our read and write
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("stale draft written: %q", m)
	}
	if a, ok := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]["draft"]; ok {
		t.Fatalf("alarm after one stale round: %+v", a)
	}
	// GitHub went to draft too: both moved alike, nothing to write, no alarm.
	w.round()
	if m := w.mutations(); len(m) != 0 || !w.formGH("widget-pr-7").Draft {
		t.Fatalf("both moved alike: mutations %q, form draft %v", m, w.formGH("widget-pr-7").Draft)
	}
	// Control: no move between read and write.
	c := newWriteWorld(t, explicitPRs)
	c.edit("widget-pr-7", addLabel("needs-review"))
	c.round()
	if m := c.mutations(); len(m) != 1 {
		t.Fatalf("control: %q", m)
	}
}

// C3b: a draft push whose base moves at both of two tries is given up (MaxTries = 2): write-refused, GitHub's
// value, the form's in sync.conflicts.draft, one wake. RED if MaxTries changes or the give-up goes.
func TestAPushStaleTwiceIsGivenUp(t *testing.T) {
	if MaxTries != 2 {
		t.Fatalf("MaxTries %d", MaxTries)
	}
	w := newWriteWorld(t, explicitPRs)
	pr7 := w.gh.prs["acme/widget"][7]
	w.edit("widget-pr-7", func(g map[string]any) { g["draft"] = true })
	// GitHub's draft moves away and back between each read and write (another tool toggling it).
	toggle := func() {
		pr7.Draft = true
		bump(pr7)
	}
	w.gh.wr.beforeFresh = toggle
	pr7After := func() { pr7.Draft = false; bump(pr7) }
	w.round()
	pr7After()
	w.gh.wr.beforeFresh = toggle
	r := w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("written: %q", m)
	}
	a, ok := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]["draft"]
	if !ok || a.Detail != "GitHub's draft moved between the read and the write, twice: GitHub's stands, the form's is in sync.conflicts.draft" {
		t.Fatalf("alarm %+v %v", a, ok)
	}
	if got := string(w.formSync("widget-pr-7").Conflicts["draft"]); got != "true" {
		t.Fatalf("sync.conflicts.draft %s", got)
	}
	if len(r.Pings) != 1 {
		t.Fatalf("pings %q", pingTexts(r))
	}
	if w.formGH("widget-pr-7").Draft {
		t.Fatalf("the form keeps the given-up wish")
	}
}

// C4: a clean write of every wave-1 field applies in one round (reply, resolve, label add and remove, draft), the
// form shows the result, and the next rounds pull nothing back and write nothing again (the reply is posted once and
// dropped from the form). RED if a pushed value is not recorded as the base (the next round pushes or reverts it).
func TestACleanWriteAppliesAndTheNextRoundPullsNothingBack(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", func(g map[string]any) {
		g["labels"] = []any{"needs-review"} // remove area:core, add needs-review
		g["draft"] = true
		thread(g, "PRRT_7_1")["resolved"] = true
		thread(g, "PRRT_7_2")["replies"] = []any{map[string]any{"body": "Fixed at 0f1e2d3; the failing case now passes."}}
	})
	w.round()
	want := []string{
		`mutation{convertPullRequestToDraft(input:{pullRequestId:"PR_7"}){pullRequest{isDraft}}}`,
		`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`,
		`mutation{removeLabelsFromLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("area:core") + `"]}){clientMutationId}}`,
		`mutation{resolveReviewThread(input:{threadId:"PRRT_7_1"}){thread{isResolved}}}`,
		`mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{state url}}} b=Fixed at 0f1e2d3; the failing case now passes.`,
	}
	if m := w.mutations(); !slices.Equal(m, want) {
		t.Fatalf("mutations\n got %q\nwant %q", m, want)
	}
	g := w.formGH("widget-pr-7")
	if !g.Draft || !slices.Equal(g.Labels, []string{"needs-review"}) || !g.Threads["PRRT_7_1"].Resolved || len(g.Threads["PRRT_7_2"].Replies) != 0 {
		t.Fatalf("form after the write: draft %v labels %v threads %+v", g.Draft, g.Labels, g.Threads)
	}
	if s := w.formSync("widget-pr-7"); len(s.Alarm) != 0 || len(s.Conflicts) != 0 {
		t.Fatalf("alarms %+v conflicts %v", s.Alarm, s.Conflicts)
	}
	for i := 0; i < 3; i++ { // the write moved updatedAt: the next due round reads it deep; nothing comes back
		w.now = w.now.Add(HotEvery)
		w.round()
		if m := w.mutations(); len(m) != 0 {
			t.Fatalf("round %d after: mutations %q", i, m)
		}
		g = w.formGH("widget-pr-7")
		if !g.Draft || !slices.Equal(g.Labels, []string{"needs-review"}) || !g.Threads["PRRT_7_1"].Resolved || g.Threads["PRRT_7_2"].N != 2 || len(g.Threads["PRRT_7_2"].Replies) != 0 {
			t.Fatalf("round %d after: draft %v labels %v threads %+v", i, g.Draft, g.Labels, g.Threads)
		}
	}
	if pr := w.gh.prs["acme/widget"][7]; !pr.Draft || len(pr.Labels.Nodes) != 1 || len(w.gh.wr.mine["PRRT_7_2"]) != 1 {
		t.Fatalf("GitHub: draft %v labels %v replies %v", pr.Draft, pr.Labels.Nodes, w.gh.wr.mine)
	}
}

// C5: a conflict (both sides moved): a reply appended while the thread was resolved on GitHub since the base is
// not posted; sync.alarm.conflict.<field> is raised with the reply in sync.conflicts, and the trusted maintainer is
// woken exactly once: the next round raises no second wake. An ack clears the alarm and wakes nobody. RED if the
// wake repeats, goes to anyone but the trusted maintainer, or the reply is posted.
func TestAConflictRaisesOneAlarmAndOneWake(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	pr7 := w.gh.prs["acme/widget"][7]
	pr7.Threads.Nodes[0].Resolved = true // resolved on GitHub after the base
	bump(pr7)
	w.edit("widget-pr-7", func(g map[string]any) {
		thread(g, "PRRT_7_1")["replies"] = []any{map[string]any{"body": "One more note on this."}}
	})
	r := w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("posted on a thread resolved upstream: %q", m)
	}
	field := "thread-PRRT_7_1-replies"
	s := w.formSync("widget-pr-7")
	if a := s.Alarm[AlarmConflict][field]; a.Detail != "the thread was resolved on GitHub since the base: reply not posted (in sync.conflicts)" || a.FormRef == "" || a.GHRef == "" {
		t.Fatalf("alarm %+v", s.Alarm)
	}
	if got := string(s.Conflicts[field]); got != `{"body":"One more note on this."}` {
		t.Fatalf("sync.conflicts %s", got)
	}
	want := []string{trustedMaint + " ghcache alarm on ebac-ghc-widget-pr-7 (@widget-pr-7): sync.alarm.conflict." + field + ": the thread was resolved on GitHub since the base: reply not posted (in sync.conflicts)"}
	if got := pingTexts(r); !slices.Equal(got, want) {
		t.Fatalf("pings\n got %q\nwant %q", got, want)
	}
	if g := w.formGH("widget-pr-7"); !g.Threads["PRRT_7_1"].Resolved || len(g.Threads["PRRT_7_1"].Replies) != 0 {
		t.Fatalf("form thread %+v", g.Threads["PRRT_7_1"])
	}
	bump(pr7) // something else moves on the PR: a deep read, the alarm stands, no second wake
	r = w.round()
	if len(r.Pings) != 0 || w.formSync("widget-pr-7").Alarm[AlarmConflict][field].Detail == "" {
		t.Fatalf("next round: pings %q alarm %+v", pingTexts(r), w.formSync("widget-pr-7").Alarm)
	}
	w.ack("widget-pr-7", AlarmConflict, field)
	r = w.round()
	if s := w.formSync("widget-pr-7"); len(r.Pings) != 0 || len(s.Alarm) != 0 || len(s.Conflicts) != 0 {
		t.Fatalf("after the ack: pings %q alarm %+v conflicts %v", pingTexts(r), s.Alarm, s.Conflicts)
	}
}

// C6: read-only fields edited on the form are reverted to GitHub's with the reason in sync.errors, and a key that
// is not a cache field is removed; nothing is written to GitHub. Control: an untouched form is not rewritten. RED if
// the read-only check goes (the edit stays).
func TestReadOnlyFieldsEditedOnTheFormAreReverted(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.round()
	if len(w.r.Forms) != 0 {
		t.Fatalf("control: an untouched round wrote %q", keys(w.r))
	}
	w.edit("widget-pr-7", func(g map[string]any) {
		g["title"] = "retitled on the form"
		g["mergeable"] = "MERGEABLE_NOW"
		g["sneaky"] = 1
	})
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("mutations %q", m)
	}
	g := w.formGH("widget-pr-7")
	if g.Title != "feat: change 7" || g.Mergeable != "MERGEABLE" {
		t.Fatalf("not reverted: %q %q", g.Title, g.Mergeable)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.forms["widget-pr-7"]["gh"], &raw)
	if _, ok := raw["sneaky"]; ok {
		t.Fatalf("an unknown gh key stayed")
	}
	if errs := w.formSync("widget-pr-7").Errors; !slices.Equal(errs, []string{"read-only gh.mergeable, gh.title edited on the form: reverted to GitHub's", "gh.sneaky: not a GitHub cache field: removed"}) {
		t.Fatalf("sync.errors %q", errs)
	}
}

// C7: a write that does not land (a seat edited the form between the core's read and its write: Expect fails) does
// not advance the base: GitHub's pulled change is pulled again next round, never pushed back as if the seat had
// removed it. RED if an unlanded round's base is promoted (the next round removes the label on GitHub).
func TestAnUnlandedPullIsNeverPushedBack(t *testing.T) {
	for _, key := range []string{"gh", "sync"} {
		t.Run(key, func(t *testing.T) { unlandedPull(t, key) })
	}
}

func unlandedPull(t *testing.T, key string) {
	w := newWriteWorld(t, explicitPRs)
	w.moveKey = key
	pr7 := w.gh.prs["acme/widget"][7]
	pr7.Labels.Nodes = append(pr7.Labels.Nodes, struct {
		Name string `json:"name"`
	}{"blocked"})
	pr7.Labels.TotalCount++
	bump(pr7)
	w.moveAt = "widget-pr-7"
	w.round()
	if w.gh.landed != 0 || slices.Contains(w.formGH("widget-pr-7").Labels, "blocked") {
		t.Fatalf("the write landed (landed %d, labels %v)", w.gh.landed, w.formGH("widget-pr-7").Labels)
	}
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("GitHub's label was pushed back: %q", m)
	}
	if got := w.formGH("widget-pr-7").Labels; !slices.Equal(got, []string{"area:core", "blocked"}) {
		t.Fatalf("form labels %v", got)
	}
}

// C8: a reply is posted once: a round whose state never saved (the plugin's reply lost) re-runs against the same
// form, and the re-read sees the operator's own comment with that body, so it is not posted again. RED if the
// viewerDidAuthor check goes (two posts).
func TestAReplyIsNeverPostedTwice(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", func(g map[string]any) {
		thread(g, "PRRT_7_2")["replies"] = []any{map[string]any{"body": "Done in the next commit."}}
	})
	before, forms := w.st, map[string]json.RawMessage{}
	for k, v := range w.forms["widget-pr-7"] {
		forms[k] = v
	}
	landed := w.gh.landed
	w.round()
	if m := w.mutations(); len(m) != 1 {
		t.Fatalf("first: %q", m)
	}
	w.st, w.forms["widget-pr-7"], w.gh.landed, w.now = before, forms, landed, w.now.Add(-HotEvery) // the round is lost
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("posted again: %q", m)
	}
	if got := w.gh.wr.mine["PRRT_7_2"]; len(got) != 1 {
		t.Fatalf("thread comments %q", got)
	}
}

// C9: reply bodies are linted like every GHE post (sdk/disclose); a body naming an internal id pattern is refused
// with the lint's reason. A label GitHub does not have is refused, not created. RED if either check goes.
func TestARefusedBodyAndAnUnknownLabelAreAlarms(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", func(g map[string]any) {
		thread(g, "PRRT_7_1")["replies"] = []any{map[string]any{"body": "0a1b2c3d is the seat that ran it"}}
		g["labels"] = append(g["labels"].([]any), "no-such-label")
	})
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("mutations %q", m)
	}
	al := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]
	if !strings.HasPrefix(al["thread-PRRT_7_1-replies"].Detail, `body names an internal term "0a1b2c3d is the seat"`) {
		t.Errorf("reply alarm %+v", al["thread-PRRT_7_1-replies"])
	}
	if al["labels"].Detail != "label(s) no-such-label do not exist in acme/widget: nothing written" {
		t.Errorf("label alarm %+v", al["labels"])
	}
}

// C10: more than MaxAlarmPings (3) new alarms in one round are one collect ping; none without a trusted maintainer
// (logged). RED if MaxAlarmPings changes or collect mode goes (four pings).
func TestAnAlarmStormIsOneCollectPing(t *testing.T) {
	if MaxAlarmPings != 3 {
		t.Fatalf("MaxAlarmPings %d", MaxAlarmPings)
	}
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-8", func(g map[string]any) {
		g["labels"] = []any{"blocked"}
		g["draft"] = true
		thread(g, "PRRT_8_1")["resolved"] = true
		thread(g, "PRRT_8_2")["resolved"] = true
	})
	r := w.round()
	if len(r.Pings) != 1 || !strings.HasPrefix(r.Pings[0].Text, "ghcache: 4 new sync alarms this round (collect mode): ebac-ghc-widget-pr-8 (@widget-pr-8) write-refused.") {
		t.Fatalf("pings %q", pingTexts(r))
	}
	n := newWriteWorld(t, explicitPRs)
	n.critic.Maintainer = ""
	n.edit("widget-pr-8", addLabel("blocked"))
	r = n.round()
	if len(r.Pings) != 0 || !strings.Contains(strings.Join(r.Log, "\n"), "1 new sync alarm(s), no trusted maintainer to wake") {
		t.Fatalf("no maintainer: pings %q log %q", pingTexts(r), r.Log)
	}
}

// The shadow critic (no trusted write) writes nothing: a seat's edit is refused with the read-only reason. RED if
// write=false stops refusing.
func TestAShadowCriticWritesNothing(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.critic.Write = false
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("mutations %q", m)
	}
	if a := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]["labels"]; a.Detail != "this critic is read-only: write comes from ebac's trusted config (authority.write), not from its form" {
		t.Fatalf("alarm %+v", a)
	}
}

// A reply posted in a round whose form write did not land stays on the form; the next round knows it was posted
// (Entity.Sent) and neither posts it nor re-reads GitHub for it. RED if Sent goes (the next round re-reads).
func TestAPostedReplyLeftOnTheFormIsNotReRead(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", func(g map[string]any) {
		thread(g, "PRRT_7_2")["replies"] = []any{map[string]any{"body": "Done in the next commit."}}
	})
	w.moveAt, w.moveKey = "widget-pr-7", "sync"
	w.round()
	if m := w.mutations(); len(m) != 1 || len(w.formGH("widget-pr-7").Threads["PRRT_7_2"].Replies) != 1 {
		t.Fatalf("first: mutations %q, form replies %+v", m, w.formGH("widget-pr-7").Threads["PRRT_7_2"])
	}
	before := len(w.gh.calls)
	w.round()
	for _, q := range w.gh.calls[before:] {
		if strings.HasPrefix(q, "{ pre: node(") {
			t.Fatalf("re-read for a reply already posted: %s", q)
		}
	}
	if m := w.mutations(); len(m) != 0 || len(w.formGH("widget-pr-7").Threads["PRRT_7_2"].Replies) != 0 {
		t.Fatalf("second: mutations %q, form replies %+v", m, w.formGH("widget-pr-7").Threads["PRRT_7_2"])
	}
}

// A form written before the write path has no base: its first round with the core's read of it is a deep read
// that agrees one, so a seat's edit right after is a push, not lost. RED if the no-base redo goes (no deep read;
// the edit waits for the next GitHub change and is then pulled over).
func TestAFormWithNoBaseIsReadDeepAtOnce(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.round() // the base lands
	if w.st.Entities["widget-pr-7"].Base == nil {
		t.Fatal("fixture: no base after two rounds")
	}
	for _, e := range w.st.Entities {
		e.Base, e.RO, e.Pend = nil, "", nil // as a form written before the write path
	}
	before := len(w.gh.calls)
	w.round()
	deep := 0
	for _, q := range w.gh.calls[before:] {
		if strings.Contains(q, "fragment D on PullRequest") {
			deep++
		}
	}
	if deep != 1 || w.st.Entities["widget-pr-7"].Base == nil && w.st.Entities["widget-pr-7"].Pend == nil {
		t.Fatalf("deep reads %d, base %+v pend %+v", deep, w.st.Entities["widget-pr-7"].Base, w.st.Entities["widget-pr-7"].Pend)
	}
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.round()
	if m := w.mutations(); len(m) != 1 {
		t.Fatalf("the edit after the base: %q", m)
	}
}

// A PR with more labels than the read holds refuses a label write (it could drop the unread ones); one write
// past MaxWrites waits for the next round on the form. The constants are pinned. RED if either guard goes or a
// constant changes.
func TestLabelTruncationAndTheWriteCap(t *testing.T) {
	if MaxWrites != 20 || MaxSent != 50 {
		t.Fatalf("MaxWrites %d MaxSent %d", MaxWrites, MaxSent)
	}
	w := newWriteWorld(t, explicitPRs)
	w.gh.prs["acme/widget"][7].Labels.TotalCount = 21 // GitHub has more than the read returned
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.round()
	if m := w.mutations(); len(m) != 0 || w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]["labels"].Detail != "the PR has 21 labels, more than the 1 read: a label write could drop the rest" {
		t.Fatalf("truncated: mutations %q alarm %+v", m, w.formSync("widget-pr-7").Alarm)
	}
	c := newWriteWorld(t, explicitPRs)
	var rs []any
	for i := 0; i < MaxWrites+1; i++ {
		rs = append(rs, map[string]any{"body": fmt.Sprintf("note %d", i)})
	}
	c.edit("widget-pr-7", func(g map[string]any) { thread(g, "PRRT_7_1")["replies"] = rs })
	c.round()
	if m := c.mutations(); len(m) != 0 || len(c.formGH("widget-pr-7").Threads["PRRT_7_1"].Replies) != MaxWrites+1 {
		t.Fatalf("over the cap: mutations %d, replies kept %d", len(m), len(c.formGH("widget-pr-7").Threads["PRRT_7_1"].Replies))
	}
	c.edit("widget-pr-7", func(g map[string]any) { thread(g, "PRRT_7_1")["replies"] = rs[:MaxWrites] })
	c.round()
	if m := c.mutations(); len(m) != MaxWrites {
		t.Fatalf("at the cap: %d mutations", len(m))
	}
}

// An alarm clears when the seat fixes the field and its push goes through: the write-refused alarm and its
// sync.conflicts value are gone and nobody is woken. RED if a clean push stops settling its field's alarms.
func TestAFixedFieldClearsItsAlarm(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", addLabel("no-such-label"))
	w.round()
	if _, ok := w.formSync("widget-pr-7").Alarm[AlarmWriteRefused]["labels"]; !ok {
		t.Fatalf("no alarm: %+v", w.formSync("widget-pr-7").Alarm)
	}
	w.edit("widget-pr-7", addLabel("needs-review"))
	r := w.round()
	if m := w.mutations(); len(m) != 1 {
		t.Fatalf("the fix: %q", m)
	}
	if s := w.formSync("widget-pr-7"); len(s.Alarm) != 0 || len(s.Conflicts) != 0 || len(r.Pings) != 0 {
		t.Fatalf("after the fix: alarm %+v conflicts %v pings %q", s.Alarm, s.Conflicts, pingTexts(r))
	}
}

func copyPR(t *testing.T, pr *PR) *PR {
	t.Helper()
	var c PR
	if err := json.Unmarshal(mustJSON(pr), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// The base check of a write compares against a FRESH GitHub read at write time, never the
// cache's state (the shadow showed mergeable, merge_state and checks stale). The cache's deep read lags: it still
// shows draft off and no "blocked" label while GitHub has both; the seat's draft and label writes are refused by
// the fresh read, nothing is written and the wishes wait on the form. Control: a cache stale only in mergeable,
// merge_state and checks, with GitHub's write fields equal to the base: the same writes apply. RED if the pre-write
// read goes (the refused writes go out) or the base check reads the cache's merge fields (the control refuses).
func TestAWriteOnAStaleCacheIsRefusedByTheFreshRead(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	pr7 := w.gh.prs["acme/widget"][7]
	w.gh.wr.staleDeep = map[int]*PR{7: copyPR(t, pr7)}
	pr7.Draft = true
	pr7.Labels.Nodes = append(pr7.Labels.Nodes, struct {
		Name string `json:"name"`
	}{"blocked"})
	pr7.Labels.TotalCount++
	bump(pr7)
	w.edit("widget-pr-7", func(g map[string]any) {
		g["draft"] = true
		g["labels"] = append(g["labels"].([]any), "needs-review")
	})
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("written on a stale cache: %q", m)
	}
	g := w.formGH("widget-pr-7")
	if !g.Draft || !slices.Equal(g.Labels, []string{"area:core", "needs-review"}) || len(w.formSync("widget-pr-7").Alarm) != 0 {
		t.Fatalf("the wishes: draft %v labels %v alarms %+v", g.Draft, g.Labels, w.formSync("widget-pr-7").Alarm)
	}

	c := newWriteWorld(t, explicitPRs)
	cpr := c.gh.prs["acme/widget"][7]
	stale := copyPR(t, cpr)
	stale.Mergeable, stale.MergeSt = "CONFLICTING", "DIRTY" // the cache's merge fields lag; the write fields do not
	c.gh.wr.staleDeep = map[int]*PR{7: stale}
	c.edit("widget-pr-7", func(g map[string]any) {
		g["draft"] = true
		g["labels"] = append(g["labels"].([]any), "needs-review")
	})
	c.round()
	want := []string{
		`mutation{convertPullRequestToDraft(input:{pullRequestId:"PR_7"}){pullRequest{isDraft}}}`,
		`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`,
	}
	if m := c.mutations(); !slices.Equal(m, want) {
		t.Fatalf("control: mutations %q", m)
	}
}

// Our label write reached GitHub, then the seat edited the form back before the form write landed
// (Expect fails). GitHub holds what we wrote, so that is the base: the form's older value is a new wish and is
// written back, never pulled over. Control: no edit, the write lands and nothing follows. RED if an unlanded round
// drops its pushed fields with the pulls (the next round pulls the cancelled label back, no mutation).
func TestSuccessfulPushThenConcurrentCancellationIsPreserved(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.moveAt = "widget-pr-7"
	w.moveFn = func() {
		w.edit("widget-pr-7", func(g map[string]any) { g["labels"] = []any{"area:core"} }) // cancelled before landing
	}
	w.round()
	if m := w.mutations(); len(m) != 1 || w.gh.landed != 0 {
		t.Fatalf("round 1: mutations %q landed %d", m, w.gh.landed)
	}
	w.round()
	want := []string{`mutation{removeLabelsFromLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("needs-review") + `"]}){clientMutationId}}`}
	if m := w.mutations(); !slices.Equal(m, want) {
		t.Fatalf("round 2: mutations %q, want the cancellation written", m)
	}
	if got := w.formGH("widget-pr-7").Labels; !slices.Equal(got, []string{"area:core"}) {
		t.Fatalf("form labels %v", got)
	}
	if n := len(w.gh.prs["acme/widget"][7].Labels.Nodes); n != 1 {
		t.Fatalf("GitHub has %d labels", n)
	}
	c := newWriteWorld(t, explicitPRs)
	c.edit("widget-pr-7", addLabel("needs-review"))
	c.round()
	c.round()
	if m := c.mutations(); len(m) != 1 || !slices.Equal(c.formGH("widget-pr-7").Labels, []string{"area:core", "needs-review"}) {
		t.Fatalf("control: %q %v", m, c.formGH("widget-pr-7").Labels)
	}
}

// A held form missing gh (or sync) when the core read it: a seat that creates the key before the
// write lands moves the form (ExpectAbsent), and the seat's key is not overwritten. Control: the key stays absent
// and the write initializes it. RED if absent keys are not expected (the pull overwrites the seat's new key).
func TestAbsentEditableKeysAreExpectedAtLanding(t *testing.T) {
	for _, key := range []string{"gh", "sync"} {
		t.Run(key, func(t *testing.T) {
			w := newWriteWorld(t, explicitPRs)
			delete(w.forms["widget-pr-7"], key)
			bump(w.gh.prs["acme/widget"][7])
			seat := json.RawMessage(`{"seat":"created between the read and the write"}`)
			w.moveAt = "widget-pr-7"
			w.moveFn = func() { w.forms["widget-pr-7"][key] = seat }
			w.round()
			if w.gh.landed != 0 || string(w.forms["widget-pr-7"][key]) != string(seat) {
				t.Fatalf("landed %d, %s = %s", w.gh.landed, key, w.forms["widget-pr-7"][key])
			}
			c := newWriteWorld(t, explicitPRs)
			delete(c.forms["widget-pr-7"], key)
			bump(c.gh.prs["acme/widget"][7])
			c.round()
			if _, ok := c.forms["widget-pr-7"][key]; !ok || c.gh.landed == 0 {
				t.Fatalf("control: %s not initialized (landed %d)", key, c.gh.landed)
			}
		})
	}
}

// An unlanded round promotes only the labels it wrote, not GitHub's whole set. B=[area:core], the form
// adds needs-review, someone adds blocked upstream before our deep read; we write needs-review, the form write does
// not land (a sync edit). Next round blocked must stay on GitHub (it is GitHub's, pulled), needs-review stays, and
// nothing is removed. RED if the whole set is promoted (blocked reads as a seat removal and is deleted).
func TestAnUnlandedPushPromotesOnlyItsOwnLabels(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	pr7 := w.gh.prs["acme/widget"][7]
	pr7.Labels.Nodes = append(pr7.Labels.Nodes, struct {
		Name string `json:"name"`
	}{"blocked"})
	pr7.Labels.TotalCount++
	bump(pr7)
	w.edit("widget-pr-7", addLabel("needs-review"))
	w.moveAt, w.moveKey = "widget-pr-7", "sync"
	w.round()
	if m := w.mutations(); len(m) != 1 || w.gh.landed != 0 {
		t.Fatalf("round 1: mutations %q landed %d", m, w.gh.landed)
	}
	w.round()
	if m := w.mutations(); len(m) != 0 {
		t.Fatalf("round 2 wrote %q (the upstream label must not be removed)", m)
	}
	var names []string
	for _, l := range pr7.Labels.Nodes {
		names = append(names, l.Name)
	}
	if !slices.Equal(sortedSet(names), []string{"area:core", "blocked", "needs-review"}) || !slices.Equal(w.formGH("widget-pr-7").Labels, []string{"area:core", "blocked", "needs-review"}) {
		t.Fatalf("GitHub %v form %v", names, w.formGH("widget-pr-7").Labels)
	}
}

// The removal half: our write removed a label, then the seat put it back before the form write landed.
// The base carries the removal (GitHub has no such label), so the seat's re-add is written back, not pulled away.
// RED if the pushed removals are dropped from the base (mutant S52: the form loses the label, nothing is written).
func TestAnUnlandedPushedRemovalThenReAddIsWritten(t *testing.T) {
	w := newWriteWorld(t, explicitPRs)
	w.edit("widget-pr-7", func(g map[string]any) { g["labels"] = []any{} })
	w.moveAt = "widget-pr-7"
	w.moveFn = func() { w.edit("widget-pr-7", func(g map[string]any) { g["labels"] = []any{"area:core"} }) }
	w.round()
	if m := w.mutations(); len(m) != 1 || w.gh.landed != 0 {
		t.Fatalf("round 1: mutations %q landed %d", m, w.gh.landed)
	}
	w.round()
	want := []string{`mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["` + labelID("area:core") + `"]}){clientMutationId}}`}
	if m := w.mutations(); !slices.Equal(m, want) || !slices.Equal(w.formGH("widget-pr-7").Labels, []string{"area:core"}) {
		t.Fatalf("round 2: mutations %q form %v", m, w.formGH("widget-pr-7").Labels)
	}
}
