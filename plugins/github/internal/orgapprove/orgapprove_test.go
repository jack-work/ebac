package orgapprove

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"github.com/gluck/ebac/sdk/grants"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/plugins/github/internal/approve"
	"github.com/gluck/ebac/sdk/model"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const headA = "aaaaaaa1111111111111111111111111111111111"
const headB = "bbbbbbb2222222222222222222222222222222222"

// fakeRead answers the lean search and the deep reads from fixtures.
type fakeRead struct {
	lean    map[string][]map[string]any // login -> nodes
	deep    map[int]map[string]any
	queries []string
}

func (f *fakeRead) Query(_ context.Context, q string) (json.RawMessage, error) {
	f.queries = append(f.queries, q)
	if strings.Contains(q, "search(") {
		d := map[string]any{}
		for i := 0; ; i++ {
			alias := fmt.Sprintf("u%d:", i)
			j := strings.Index(q, alias)
			if j < 0 {
				break
			}
			rest := q[j:]
			k := strings.Index(rest, "author:")
			login := rest[k+len("author:"):]
			login = login[:strings.IndexAny(login, "\\\"")]
			d[fmt.Sprintf("u%d", i)] = map[string]any{"nodes": f.lean[login]}
		}
		return json.Marshal(d)
	}
	repo := map[string]any{}
	for n, p := range f.deep {
		if strings.Contains(q, fmt.Sprintf("p%d:", n)) {
			repo[fmt.Sprintf("p%d", n)] = p
		}
	}
	return json.Marshal(map[string]any{"repository": repo})
}

func (f *fakeRead) Spent() approve.Spend {
	return approve.Spend{Calls: len(f.queries), Points: len(f.queries), Remaining: 4000}
}

type fakeWrite struct {
	info     approve.PRInfo
	reviews  []approve.Review
	approved []int
	lost     bool  // Approve "succeeds" but no review appears
	readErr  error // every review read after the first Approve fails
}

func (f *fakeWrite) PR(context.Context, int) (approve.PRInfo, error) { return f.info, nil }
func (f *fakeWrite) Reviews(context.Context, int) ([]approve.Review, error) {
	if f.readErr != nil && len(f.approved) > 0 {
		return nil, f.readErr
	}
	return f.reviews, nil
}
func (f *fakeWrite) Approve(_ context.Context, n int, body string) error {
	f.approved = append(f.approved, n)
	if f.lost {
		return nil
	}
	r := approve.Review{ID: 77, State: "APPROVED", Body: body}
	r.User.Login, r.User.Type = Self, "User"
	f.reviews = append(f.reviews, r)
	return nil
}

type fakeFile struct {
	filed  []string // reasons
	states map[string]string
	next   int
}

func (f *fakeFile) File(_ context.Context, cred string, ttl time.Duration, reason string) (string, error) {
	if cred != "ebac-approve.org" || ttl != AskTTL {
		return "", fmt.Errorf("filed %s for %s", cred, ttl)
	}
	f.next++
	f.filed = append(f.filed, reason)
	return fmt.Sprintf("%012x", f.next), nil
}

func (f *fakeFile) State(_ context.Context, id string) (grants.RequestState, error) {
	s := f.states[id]
	if s == "" {
		s = "pending"
	}
	return grants.RequestState{ID: id, State: s}, nil
}

func leanPR(n int, login, head string, created time.Time, draft bool) map[string]any {
	return map[string]any{"number": n, "title": fmt.Sprintf("PR %d", n), "createdAt": created.Format(time.RFC3339),
		"isDraft": draft, "headRefOid": head, "author": map[string]any{"login": login}}
}

func who(l string) map[string]any {
	t := "User"
	if l == "automaton" || strings.HasSuffix(l, "[bot]") {
		t = "Bot"
	}
	return map[string]any{"login": l, "__typename": t}
}

func nodes(xs ...map[string]any) map[string]any { return map[string]any{"nodes": xs} }

// deepPR is a ready PR: 3 commits, one bot thread answered by the author,
// checks green, no human CR, not draft, mergeable.
func deepPR(n int, author, head string) map[string]any {
	return map[string]any{
		"number": n, "isDraft": false, "headRefOid": head, "reviewDecision": "REVIEW_REQUIRED", "mergeable": "MERGEABLE",
		"author":  map[string]any{"login": author},
		"commits": map[string]any{"totalCount": 2}, "force": map[string]any{"totalCount": 1},
		"last": nodes(map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "SUCCESS",
			"contexts": nodes(map[string]any{"name": "build", "conclusion": "SUCCESS"})}}}),
		"reviews": nodes(map[string]any{"state": "COMMENTED", "author": who("automaton")}),
		"reviewThreads": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"isResolved": false,
			"first": nodes(map[string]any{"author": who("automaton")}), "lastc": nodes(map[string]any{"author": who(author)})}}},
		"files": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"path": "src/lib.rs"}}},
	}
}

type rig struct {
	rd    *fakeRead
	wr    *fakeWrite
	fl    *fakeFile
	st    *model.OrgApproveState
	w     model.OrgApproveWatch
	grant []grants.Grant
	now   time.Time
}

func newRig() *rig {
	r := &rig{
		rd: &fakeRead{lean: map[string][]map[string]any{"slahoti": {leanPR(115803, "slahoti", headA, t0.Add(-5*24*time.Hour), false)}},
			deep: map[int]map[string]any{115803: deepPR(115803, "slahoti", headA)}},
		wr:  &fakeWrite{info: approve.PRInfo{State: "open"}},
		fl:  &fakeFile{states: map[string]string{}},
		st:  &model.OrgApproveState{},
		w:   model.OrgApproveWatch{Logins: []string{"slahoti", "John-Kelliher"}, Grant: "ebac-approve.org", Principal: "org-approve", Approver: "jack", PingTo: "@ping"},
		now: t0,
	}
	r.wr.info.Head.SHA = headA
	return r
}

func (r *rig) round(t *testing.T, dry bool) Out {
	t.Helper()
	o, err := Run(context.Background(), r.rd, r.wr, r.fl, Pass{Watch: r.w, State: r.st, Now: r.now, DryRun: dry,
		Grants: func() ([]grants.Grant, error) { return r.grant, nil }})
	if err != nil {
		t.Fatalf("round: %v (log %v)", err, o.Log)
	}
	return o
}

func TestLeanQueryOneAliasPerLoginSelfExcluded(t *testing.T) {
	r := newRig()
	r.w.Logins = []string{"slahoti", "John-Kelliher", "bad login", "ahkotb"}
	r.round(t, true)
	q := r.rd.queries[0]
	if strings.Count(q, "search(") != 2 || strings.Contains(q, "John-Kelliher") || strings.Contains(q, "bad login") {
		t.Fatalf("lean query: %s", q)
	}
	if strings.Contains(q, "body") || strings.Contains(q, "comments") {
		t.Fatalf("lean query reads bodies: %s", q)
	}
}

func TestCandidateAskedOnceWithRequestAndBoardQ(t *testing.T) {
	r := newRig()
	o := r.round(t, false)
	id := QID(115803, headA)
	if id != "oa-115803-aaaaaaa" {
		t.Fatalf("qid %s", id)
	}
	if len(r.fl.filed) != 1 || r.fl.filed[0] != "APPROVE bic/aether#115803 at aaaaaaa" {
		t.Fatalf("filed %v", r.fl.filed)
	}
	q := o.Q[id]
	if q == nil || q["status"] != "open" || !strings.Contains(q["context"].(string), "000000000001") || !strings.Contains(q["context"].(string), grants.Page) {
		t.Fatalf("q %v", q)
	}
	for f := range q {
		if f == "answer" || f == "elaborate" {
			t.Fatalf("asker wrote the Boss's field %s", f)
		}
	}
	if len(o.Pings) != 1 || o.Pings[0].To != "@ping" {
		t.Fatalf("pings %v", o.Pings)
	}
	// Second round, nothing approved: no new request, no post, no deep re-read.
	n := len(r.rd.queries)
	o = r.round(t, false)
	if len(r.fl.filed) != 1 || len(r.wr.approved) != 0 || len(o.Q) != 0 || len(r.rd.queries) != n+1 {
		t.Fatalf("second round: filed %v approved %v q %v queries %d", r.fl.filed, r.wr.approved, o.Q, len(r.rd.queries)-n)
	}
}

func (r *rig) approveTap(by string) {
	r.fl.states["000000000001"] = "approved"
	r.grant = []grants.Grant{{ID: "000000000001", Principal: "org-approve", Credential: "ebac-approve.org", ExpiresAt: r.now.Add(time.Hour).Unix(), ApprovedBy: by}}
}

func TestTapByJackApprovesAtHead(t *testing.T) {
	r := newRig()
	r.round(t, false)
	r.approveTap("jack")
	r.now = r.now.Add(15 * time.Minute)
	o := r.round(t, false)
	if len(r.wr.approved) != 1 || r.wr.approved[0] != 115803 {
		t.Fatalf("approved %v (log %v)", r.wr.approved, o.Log)
	}
	q := o.Q[QID(115803, headA)]
	if q["status"] != "done" || !strings.Contains(q["resolved"].(string), "review 77") {
		t.Fatalf("q %v", q)
	}
	// Once only.
	o = r.round(t, false)
	if len(r.wr.approved) != 1 || len(r.fl.filed) != 1 {
		t.Fatalf("re-approved or re-asked: %v %v", r.wr.approved, r.fl.filed)
	}
}

func TestNoPostWithoutJacksApprovalOfThatRequest(t *testing.T) {
	cases := map[string]func(r *rig){
		"standing flake":    func(r *rig) { r.approveTap("flake:cheroot") },
		"another approver":  func(r *rig) { r.approveTap("gluck") },
		"other credential":  func(r *rig) { r.approveTap("jack"); r.grant[0].Credential = "ebac-approve.aether" },
		"other principal":   func(r *rig) { r.approveTap("jack"); r.grant[0].Principal = "sbx-cheroot-sec" },
		"other request row": func(r *rig) { r.approveTap("jack"); r.grant[0].ID = "000000000009" },
		"expired":           func(r *rig) { r.approveTap("jack"); r.grant[0].ExpiresAt = r.now.Unix() },
		"pending":           func(r *rig) { r.approveTap("jack"); r.fl.states["000000000001"] = "pending" },
		"denied":            func(r *rig) { r.approveTap("jack"); r.fl.states["000000000001"] = "denied" },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig()
			r.round(t, false)
			set(r)
			o := r.round(t, false)
			if len(r.wr.approved) != 0 {
				t.Fatalf("posted (log %v)", o.Log)
			}
		})
	}
}

func TestPostTimeChecksRefuse(t *testing.T) {
	cases := map[string]func(r *rig){
		"head moved on GHE": func(r *rig) { r.wr.info.Head.SHA = headB },
		"closed":            func(r *rig) { r.wr.info.State = "closed" },
		"draft":             func(r *rig) { r.wr.info.Draft = true },
		"human CR": func(r *rig) {
			rv := approve.Review{ID: 5, State: "CHANGES_REQUESTED"}
			rv.User.Login, rv.User.Type = "someone", "User"
			r.wr.reviews = append(r.wr.reviews, rv)
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig()
			r.round(t, false)
			r.approveTap("jack")
			set(r)
			o := r.round(t, false)
			if len(r.wr.approved) != 0 {
				t.Fatalf("posted (log %v)", o.Log)
			}
			if q := o.Q[QID(115803, headA)]; q == nil || q["status"] != "withdrawn" {
				t.Fatalf("q %v", q)
			}
		})
	}
}

func TestHeadMoveWithdrawsAndReasksAtNewHead(t *testing.T) {
	r := newRig()
	r.round(t, false)
	r.rd.lean["slahoti"][0]["headRefOid"] = headB
	r.rd.deep[115803] = deepPR(115803, "slahoti", headB)
	r.approveTap("jack") // approved for the OLD head: must not post, even if
	// the REST read still shows the old head (GHE's views lag each other)
	o := r.round(t, false)
	if len(r.wr.approved) != 0 {
		t.Fatalf("posted at a moved head")
	}
	if q := o.Q[QID(115803, headA)]; q == nil || q["status"] != "withdrawn" {
		t.Fatalf("old q %v", q)
	}
	if q := o.Q[QID(115803, headB)]; q == nil || q["status"] != "open" || len(r.fl.filed) != 2 {
		t.Fatalf("new q %v filed %v", q, r.fl.filed)
	}
}

func TestDryRunFilesAndPostsNothing(t *testing.T) {
	r := newRig()
	o := r.round(t, true)
	if len(r.fl.filed) != 0 || len(o.Q) != 0 {
		t.Fatalf("dry run filed %v q %v", r.fl.filed, o.Q)
	}
}

func TestOpenAsksCappedAtBandGrantsMaxPending(t *testing.T) {
	r := newRig()
	var ls []map[string]any
	for i := 0; i < 7; i++ {
		n := 200 + i
		ls = append(ls, leanPR(n, "slahoti", headA, t0.Add(-5*24*time.Hour), false))
		r.rd.deep[n] = deepPR(n, "slahoti", headA)
	}
	r.rd.lean["slahoti"] = ls
	r.round(t, false) // deep reads 4
	r.round(t, false) // deep reads 3
	if len(r.fl.filed) != MaxOpenAsks {
		t.Fatalf("filed %d, want %d", len(r.fl.filed), MaxOpenAsks)
	}
}

func scoreOf(t *testing.T, mut func(d map[string]any), breaks ...string) *model.OrgPR {
	t.Helper()
	d := deepPR(1, "a", headA)
	if mut != nil {
		mut(d)
	}
	b, _ := json.Marshal(d)
	var dp DeepPR
	if err := json.Unmarshal(b, &dp); err != nil {
		t.Fatal(err)
	}
	pr := &model.OrgPR{Number: 1, Head: headA, DeepAt: "x"}
	score(pr, &dp, breaks, t0)
	return pr
}

func TestScore(t *testing.T) {
	if p := scoreOf(t, nil); p.Score != 6 || !p.Candidate() {
		t.Fatalf("ready PR: %+v", p)
	}
	red := func(d map[string]any) {
		d["last"] = nodes(map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "FAILURE",
			"contexts": nodes(map[string]any{"name": "Main Break", "conclusion": "FAILURE"})}}})
	}
	cases := []struct {
		name string
		mut  func(map[string]any)
		fail string
	}{
		{"draft", func(d map[string]any) { d["isDraft"] = true }, "not-draft"},
		{"two iterations", func(d map[string]any) { d["force"] = map[string]any{"totalCount": 0} }, "iter>=3"},
		{"no bot review", func(d map[string]any) {
			d["reviews"] = nodes()
			d["reviewThreads"] = map[string]any{"totalCount": 0, "nodes": []any{}}
		}, "bot-reviewed+answered"},
		{"bot thread unanswered", func(d map[string]any) {
			d["reviewThreads"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["lastc"] = nodes(map[string]any{"author": who("automaton")})
		}, "bot-reviewed+answered"},
		{"standing bot CR", func(d map[string]any) {
			d["reviews"] = nodes(map[string]any{"state": "CHANGES_REQUESTED", "author": who("automaton")})
		}, "bot-reviewed+answered"},
		{"threads unread", func(d map[string]any) { d["reviewThreads"].(map[string]any)["totalCount"] = 99 }, "threads-read"},
		{"red check", red, "checks-green"},
		{"human CR", func(d map[string]any) {
			d["reviews"] = nodes(map[string]any{"state": "COMMENTED", "author": who("automaton")},
				map[string]any{"state": "CHANGES_REQUESTED", "author": who("person")})
		}, "no-human-CR"},
	}
	for _, c := range cases {
		p := scoreOf(t, c.mut)
		if p.Candidate() || !contains(p.Fails, c.fail) {
			t.Errorf("%s: score %d fails %v, want %s", c.name, p.Score, p.Fails, c.fail)
		}
	}
	if p := scoreOf(t, red, "Main Break"); !p.Candidate() {
		t.Errorf("red only on a known break: %+v", p)
	}
	for name, mut := range map[string]func(map[string]any){
		"conflicting": func(d map[string]any) { d["mergeable"] = "CONFLICTING" },
		"approved":    func(d map[string]any) { d["reviewDecision"] = "APPROVED" },
		"jack reviewed": func(d map[string]any) {
			d["reviews"] = nodes(map[string]any{"state": "COMMENTED", "author": who("automaton")},
				map[string]any{"state": "APPROVED", "author": who(Self)})
		},
	} {
		if p := scoreOf(t, mut); p.Candidate() || len(p.Extra) == 0 {
			t.Errorf("%s: not excluded: %+v", name, p)
		}
	}
	p := scoreOf(t, func(d map[string]any) {
		d["files"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"path": ".github/workflows/ci.yml"}}}
	})
	if len(p.Sens) != 1 {
		t.Errorf("sensitive path not flagged: %+v", p)
	}
}

func TestCheckWatch(t *testing.T) {
	ok := model.OrgApproveWatch{Logins: []string{"a"}, Grant: "ebac-approve.org", Principal: "org-approve", Approver: "jack"}
	if err := ghcfg.CheckOrgApprove(&ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*model.OrgApproveWatch){
		"no logins":      func(w *model.OrgApproveWatch) { w.Logins = nil },
		"bad login":      func(w *model.OrgApproveWatch) { w.Logins = []string{"a b"} },
		"glob grant":     func(w *model.OrgApproveWatch) { w.Grant = "ebac-approve.*" },
		"flake approver": func(w *model.OrgApproveWatch) { w.Approver = "flake:cheroot" },
		"empty approver": func(w *model.OrgApproveWatch) { w.Approver = "" },
		"ping not role":  func(w *model.OrgApproveWatch) { w.PingTo = "cheroot" },
	} {
		w := ok
		mut(&w)
		if ghcfg.CheckOrgApprove(&w) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A misconfigured approver (empty, or a flake owner) never takes a row as a
// tap, even one whose approved_by matches it; nor does a standing row.
func TestTapGrantRefusesNonPersonApprovers(t *testing.T) {
	live := t0.Add(time.Hour).Unix()
	w := model.OrgApproveWatch{Grant: "ebac-approve.org", Principal: "org-approve"}
	row := func(by string) []grants.Grant {
		return []grants.Grant{{ID: "000000000001", Principal: "org-approve", Credential: "ebac-approve.org", ExpiresAt: live, ApprovedBy: by}}
	}
	for _, c := range []struct{ approver, by string }{{"", ""}, {"flake:cheroot", "flake:cheroot"}, {"", "jack"}, {"flake:x", "jack"}} {
		w.Approver = c.approver
		if why := tapGrant(row(c.by), "000000000001", w, t0.Unix()); why == "" {
			t.Errorf("approver %q took a row approved by %q", c.approver, c.by)
		}
	}
	w.Approver = "jack"
	if why := tapGrant(row("jack"), "000000000001", w, t0.Unix()); why != "" {
		t.Fatalf("jack's row refused: %s", why)
	}
	if why := tapGrant(row("flake:jack"), "000000000001", w, t0.Unix()); why == "" {
		t.Error("a flake row taken")
	}
	// The whole round: a critic whose approver was emptied posts nothing.
	r := newRig()
	r.round(t, false)
	r.approveTap("")
	r.w.Approver = ""
	r.round(t, false)
	if len(r.wr.approved) != 0 {
		t.Fatal("posted with an empty approver")
	}
}

// bic/aether#129969: an approve sent but not read back (read error, or not
// seen) closes the ask as done with a ping, never retried: no second post.
func TestSentButNotReadBackIsNeverRetried(t *testing.T) {
	for name, set := range map[string]func(*fakeWrite){
		"read-back error": func(f *fakeWrite) { f.readErr = errors.New("gh: HTTP 502") },
		"not seen":        func(f *fakeWrite) { f.lost = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig()
			r.round(t, false)
			r.approveTap("jack")
			set(r.wr)
			r.now = r.now.Add(15 * time.Minute)
			o := r.round(t, false)
			q := o.Q[QID(115803, headA)]
			if len(r.wr.approved) != 1 || q == nil || q["status"] != "done" || !strings.HasPrefix(q["resolved"].(string), SentNotSeen) {
				t.Fatalf("approved %v q %v (log %v)", r.wr.approved, q, o.Log)
			}
			pinged := false
			for _, p := range o.Pings {
				pinged = pinged || strings.Contains(p.Text, "approve SENT but not read back")
			}
			if !pinged {
				t.Fatalf("no ping: %+v", o.Pings)
			}
			r.now = r.now.Add(time.Minute)
			r.round(t, false)
			if len(r.wr.approved) != 1 {
				t.Fatalf("posted again: %v", r.wr.approved)
			}
		})
	}
}
