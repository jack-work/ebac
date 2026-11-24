package approve

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"github.com/gluck/ebac/sdk/grants"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
	"github.com/gluck/ebac/sdk/tomb"
)

// Every argv any test in this package hands gh, checked in TestMain: the
// source can reach only these three shapes, whatever the requests say.
var (
	argvMu  sync.Mutex
	argvAll [][]string
	allowed = []*regexp.Regexp{
		regexp.MustCompile(`^api repos/bic/aether/pulls/[0-9]+$`),
		regexp.MustCompile(`^api repos/bic/aether/pulls/[0-9]+/reviews\?per_page=100&page=[0-9]{1,3}$`),
		regexp.MustCompile(`^pr review [0-9]+ --repo bic/aether --approve --body Approved on Jack's behalf \(Figaro\), on [A-Za-z][A-Za-z .'-]{0,60}'s ask [0-9]{10,16}\.\n\n<!-- figaro-ask:[A-Za-z0-9._-]{1,64} -->$`),
	}
)

func TestMain(m *testing.M) {
	code := m.Run()
	argvMu.Lock()
	n := len(argvAll)
	for _, a := range argvAll {
		line := strings.Join(a, " ")
		ok := false
		for _, re := range allowed {
			ok = ok || re.MatchString(line)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "FAIL: gh argv outside the three allowed shapes: %q\n", a)
			code = 1
		}
	}
	argvMu.Unlock()
	if f := flag.Lookup("test.run"); code == 0 && n < 20 && (f == nil || f.Value.String() == "") {
		// The check above proves nothing over an empty record.
		fmt.Fprintf(os.Stderr, "FAIL: only %d gh calls recorded across the suite; the argv check is blind\n", n)
		code = 1
	}
	os.Exit(code)
}

const (
	chat  = "19:54e931af-ef17-434f-8a3c-f2e04baf42f5_bc3cb0f4-1239-4ec7-a528-4913522f79c5@unq.gbl.spaces"
	ahmed = "8:orgid:54e931af-ef17-434f-8a3c-f2e04baf42f5"
	jack  = "8:orgid:bc3cb0f4-1239-4ec7-a528-4913522f79c5"
	msgID = "1794516486156"
)

var watch = model.ApproveWatch{Chat: chat, ChatAlias: "ahmed", AskFrom: ahmed, Grant: "ebac-approve.aether",
	Principal: "sbx-ops-sec", PingTo: "@000000a4", AskName: "Pat Doe"}

var now = time.Date(2026, 11, 12, 21, 0, 0, 0, time.UTC)

// fakeHub is the fake GitHub: PRs and their reviews, answering by argv.
type fakeHub struct {
	prs      map[int]PRInfo
	reviews  map[int][]Review
	readErr  error // a non-404 read failure
	postErr  error // Approve fails
	postLost bool  // Approve "succeeds" but no review appears
	// readErrAfterPost fails every read after the first Approve
	readErrAfterPost error
	calls            [][]string
	nextID           int64
}

func (h *fakeHub) gh() *GH {
	return &GH{exec: func(_ context.Context, args []string) ([]byte, error) {
		a := append([]string(nil), args...)
		h.calls = append(h.calls, a)
		argvMu.Lock()
		argvAll = append(argvAll, a)
		argvMu.Unlock()
		if a[0] == "pr" {
			n, _ := strconv.Atoi(a[2])
			if h.postErr != nil {
				return nil, h.postErr
			}
			if !h.postLost {
				h.nextID++
				h.reviews[n] = append(h.reviews[n], Review{ID: 9000 + h.nextID, State: "APPROVED", Body: a[len(a)-1],
					SubmittedAt: now.Add(time.Second).Format(time.RFC3339)})
			}
			return nil, nil
		}
		if h.readErr != nil {
			return nil, h.readErr
		}
		if h.readErrAfterPost != nil && h.posts() > 0 {
			return nil, h.readErrAfterPost
		}
		path := a[1]
		n, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(path, "repos/bic/aether/pulls/"), "/")[0])
		if strings.Contains(path, "/reviews") {
			page, _ := strconv.Atoi(path[strings.LastIndex(path, "=")+1:])
			rs := h.reviews[n]
			lo, hi := (page-1)*ReviewsPerPage, page*ReviewsPerPage
			if lo > len(rs) {
				lo = len(rs)
			}
			if hi > len(rs) {
				hi = len(rs)
			}
			return json.Marshal(append([]Review{}, rs[lo:hi]...))
		}
		pr, ok := h.prs[n]
		if !ok {
			return nil, errors.New("gh: Not Found (HTTP 404)")
		}
		return json.Marshal(pr)
	}}
}

func (h *fakeHub) posts() int {
	n := 0
	for _, c := range h.calls {
		if c[0] == "pr" {
			n++
		}
	}
	return n
}

type fakeTeams struct {
	msgs []tomb.TeamsMsg
	err  error
}

func (f fakeTeams) Messages(_ context.Context, conv string, limit int) ([]tomb.TeamsMsg, error) {
	if conv != chat || limit != AskWindow {
		return nil, fmt.Errorf("wrong chat %q or limit %d", conv, limit)
	}
	return f.msgs, f.err
}

func msg(id, from, content, arrival string) tomb.TeamsMsg {
	return tomb.TeamsMsg{ID: id, MessageType: "RichText/Html", FromURL: "https://x/v1/users/ME/contacts/" + from,
		Content: content, Arrival: arrival}
}

func liveGrants() ([]grants.Grant, error) {
	return []grants.Grant{
		{ID: "a", Principal: "sbx-ops-sec", Credential: "tomb-live.notes", ExpiresAt: now.Unix() + 3600, ApprovedBy: "flake:ops"},
		{ID: "b", Principal: "sbx-ops-sec", Credential: "ebac-approve.aether", ExpiresAt: now.Unix() + 86400, ApprovedBy: "jack"},
	}, nil
}

// world is one scenario: an open aether PR 110001 with an old approval, and
// Pat's ask naming it after that approval.
func world() (*fakeHub, fakeTeams) {
	h := &fakeHub{prs: map[int]PRInfo{110001: {State: "open"}, 110002: {State: "open", Draft: true}, 110003: {State: "closed"}},
		reviews: map[int][]Review{110001: {{ID: 1, State: "APPROVED", Body: "lgtm", SubmittedAt: "2026-11-12T19:00:00Z"},
			{ID: 2, State: "COMMENTED", Body: "nit", SubmittedAt: "2026-11-12T20:59:00Z"}}}}
	t := fakeTeams{msgs: []tomb.TeamsMsg{
		msg("1794516400000", jack, "hello", "2026-11-12T20:40:00.000Z"),
		msg(msgID, ahmed, `Jack, can you approve <a href="https://microsoft.ghe.com/bic/aether/pull/110001">this</a>?`, "2026-11-12T20:48:06.156Z"),
	}}
	return h, t
}

func reqJSON(fields map[string]any) json.RawMessage {
	b, _ := json.Marshal(fields)
	return b
}

func good() map[string]any {
	return map[string]any{"repo": "bic/aether", "pr": 110001, "teams_msg_id": msgID, "chat": "ahmed", "at": "2026-11-12T20:50:00Z", "by": "x"}
}

// ledger plays the core across rounds: each round mints a token, runs OpRound, SAVES the ledger the reply carries,
// then (Commit asked) runs OpCommit with the same token and saves again. lose drops the commit's save (the reply
// was lost after the POST); crash skips the commit call entirely (the core died after the save).
type ledger struct {
	posts       map[string]*model.PostRecord
	n           int
	lose, crash bool
}

func (l *ledger) round(t *testing.T, h *fakeHub, tm fakeTeams, reqs map[string]json.RawMessage, at time.Time, mut ...func(*Pass)) (Out, error) {
	t.Helper()
	l.n++
	p := Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: at, Op: plugin.OpRound, Token: fmt.Sprintf("%016x", l.n), Posts: l.posts}
	for _, m := range mut {
		m(&p)
	}
	out, err := Run(context.Background(), h.gh(), tm, p)
	l.posts = out.Posts
	if !out.Commit || l.crash {
		return out, err
	}
	p.Op, p.Posts = plugin.OpCommit, l.posts
	c, cerr := Run(context.Background(), h.gh(), tm, p)
	if !l.lose {
		l.posts = c.Posts
	}
	for id, r := range c.Results {
		out.Results[id] = r
	}
	out.Pings = append(out.Pings, c.Pings...)
	out.Log = append(out.Log, c.Log...)
	out.Commit = false
	return out, errors.Join(err, cerr)
}

// run is one round (both phases) on an empty ledger.
func run(t *testing.T, h *fakeHub, tm fakeTeams, reqs map[string]json.RawMessage, mut ...func(*Pass)) (Out, error) {
	t.Helper()
	return (&ledger{}).round(t, h, tm, reqs, now, mut...)
}

// decided writes a round's results onto the requests, as the core lands them.
func decided(reqs map[string]json.RawMessage, out Out) {
	for id, r := range out.Results {
		var f map[string]any
		_ = json.Unmarshal(reqs[id], &f)
		f["result"] = r
		reqs[id] = reqJSON(f)
	}
}

func TestApproveHappyPath(t *testing.T) {
	h, tm := world()
	out, err := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Results["r1"]
	if !r.OK || r.ReviewID == 0 || r.At != "2026-11-12T21:00:00Z" || h.posts() != 1 {
		t.Fatalf("result %+v, posts %d", r, h.posts())
	}
	if len(out.Pings) != 1 || out.Pings[0].To != "@000000a4" || !strings.Contains(out.Pings[0].Text, "bic/aether#110001") ||
		!strings.Contains(out.Pings[0].Text, msgID) || strings.ContainsAny(out.Pings[0].Text, "\n\r") {
		t.Fatalf("pings %+v", out.Pings)
	}
	last := h.calls[len(h.calls)-2]
	if strings.Join(last, " ") != "pr review 110001 --repo bic/aether --approve --body "+PostBody(watch.Asker(), msgID, "r1") {
		t.Fatalf("approve argv %q", last)
	}
}

// Every refusal branch: each check, named in the reason; nothing posted.
func TestApproveRefusals(t *testing.T) {
	with := func(k string, v any) json.RawMessage {
		f := good()
		if v == nil {
			delete(f, k)
		} else {
			f[k] = v
		}
		return reqJSON(f)
	}
	withGrants := func(gs ...grants.Grant) func(*Pass) {
		return func(p *Pass) { p.Grants = func() ([]grants.Grant, error) { return gs, nil } }
	}
	g := func(principal, cred string, exp int64, by string) grants.Grant {
		return grants.Grant{ID: "x", Principal: principal, Credential: cred, ExpiresAt: exp, ApprovedBy: by}
	}
	live := now.Unix() + 600
	cases := []struct {
		name, id string
		req      json.RawMessage
		mut      func(*Pass)
		teams    func(*fakeTeams)
		hub      func(*fakeHub)
		want     string
	}{
		{name: "bad id", id: "r 1", req: reqJSON(good()), want: "shape: request id"},
		{name: "unknown field event", req: with("event", "REQUEST_CHANGES"), want: `shape: unknown field "event"`},
		{name: "repo not string", req: with("repo", 7), want: "shape: repo must be a string"},
		{name: "repo missing", req: with("repo", nil), want: "shape: repo must be a string"},
		{name: "pr string", req: with("pr", "110001"), want: "shape: pr must be a number"},
		{name: "pr zero", req: with("pr", 0), want: "shape: pr must be an integer"},
		{name: "pr fraction", req: with("pr", 1.5), want: "shape: pr must be an integer"},
		{name: "pr huge", req: with("pr", 1e12), want: "shape: pr must be an integer"},
		{name: "msg id letters", req: with("teams_msg_id", "17945x6486156"), want: "shape: teams_msg_id"},
		{name: "msg id number", req: with("teams_msg_id", 1794516486156), want: "shape: teams_msg_id"},
		{name: "grants unreadable", req: reqJSON(good()), mut: func(p *Pass) {
			p.Grants = func() ([]grants.Grant, error) {
				return nil, errors.New("open /var/lib/band-grants/grants.json: no such file")
			}
		}, want: "grant: grants unreadable"},
		{name: "grant absent", req: reqJSON(good()), mut: withGrants(), want: "grant: no live grant ebac-approve.aether for sbx-ops-sec"},
		{name: "grant figaro principal", req: reqJSON(good()), mut: withGrants(g("sandbox:ops-sec@local:box", "ebac-approve.aether", live, "jack")),
			want: "grant: no live grant"},
		{name: "grant prefix only", req: reqJSON(good()), mut: withGrants(g("sbx-ops-sec", "ebac-approve.*", live, "jack")), want: "grant: no live grant"},
		{name: "grant longer name", req: reqJSON(good()), mut: withGrants(g("sbx-ops-sec", "ebac-approve.aether2", live, "jack")), want: "grant: no live grant"},
		{name: "grant other principal", req: reqJSON(good()), mut: withGrants(g("sbx-other", "ebac-approve.aether", live, "jack")), want: "grant: no live grant"},
		{name: "grant expired", req: reqJSON(good()), mut: withGrants(g("sbx-ops-sec", "ebac-approve.aether", now.Unix(), "jack")), want: "expired"},
		{name: "grant standing", req: reqJSON(good()), mut: withGrants(g("sbx-ops-sec", "ebac-approve.aether", live, "flake:ops")), want: "is standing"},
		{name: "grant no approver", req: reqJSON(good()), mut: withGrants(g("sbx-ops-sec", "ebac-approve.aether", live, "")), want: "is standing"},
		{name: "repo app-studio", req: with("repo", "bic/app-studio"), want: `scope: repo "bic/app-studio" is not bic/aether`},
		{name: "repo case", req: with("repo", "bic/Aether"), want: "scope: repo"},
		{name: "pr draft", req: with("pr", 110002), want: "is a draft"},
		{name: "pr closed", req: with("pr", 110003), want: "is closed, not open"},
		{name: "pr missing", req: with("pr", 119999), want: "not found"},
		{name: "ask missing", req: with("teams_msg_id", "1794516499999"), want: "is not among the chat's last 50"},
		{name: "ask from jack", req: with("teams_msg_id", "1794516400000"), want: "is from " + jack},
		{name: "ask deleted", req: reqJSON(good()), teams: func(f *fakeTeams) {
			f.msgs[1].Properties = map[string]any{"deletetime": "1794516490000"}
		}, want: "is not a live message"},
		{name: "ask names another PR", req: reqJSON(good()), teams: func(f *fakeTeams) { f.msgs[1].Content = "please approve #110005" }, want: "does not name"},
		{name: "ask names app-studio#N", req: reqJSON(good()), teams: func(f *fakeTeams) { f.msgs[1].Content = "approve app-studio#110001 please" },
			want: "does not name"},
		{name: "ask links another repo", req: reqJSON(good()), teams: func(f *fakeTeams) {
			f.msgs[1].Content = `<a href="https://microsoft.ghe.com/bic/app-studio/pull/110001">#110001</a>`
		}, want: "does not name"},
		{name: "ask no arrival", req: reqJSON(good()), teams: func(f *fakeTeams) { f.msgs[1].Arrival = "" }, want: "no server arrival time"},
		{name: "ask stale", req: reqJSON(good()), hub: func(h *fakeHub) {
			h.reviews[110001] = append(h.reviews[110001], Review{ID: 3, State: "APPROVED", Body: "ok", SubmittedAt: "2026-11-12T20:48:06.156Z"})
		}, want: "is not newer than approval 3"},
		// A failed or lost POST is no refusal in its round under #27 B: TestAPostErrorIsReadBackNeverRetried.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, tm := world()
			if c.teams != nil {
				tm.msgs = append([]tomb.TeamsMsg(nil), tm.msgs...)
				c.teams(&tm)
			}
			if c.hub != nil {
				c.hub(h)
			}
			id := c.id
			if id == "" {
				id = "r1"
			}
			var muts []func(*Pass)
			if c.mut != nil {
				muts = append(muts, c.mut)
			}
			out, err := run(t, h, tm, map[string]json.RawMessage{id: c.req}, muts...)
			if err != nil {
				t.Fatal(err)
			}
			r, ok := out.Results[id]
			if !ok || r.OK || !strings.Contains(r.Reason, c.want) {
				t.Fatalf("want a refusal %q, got %+v (present %v)", c.want, r, ok)
			}
			wantPings := 0
			if strings.HasPrefix(c.want, ReadBackFailed) {
				wantPings = 1 // sent but not read back: the requester is told to look
			}
			if len(out.Pings) != wantPings {
				t.Fatalf("pings %+v, want %d", out.Pings, wantPings)
			}
			wantPosts := 0
			if strings.HasPrefix(c.want, "post:") {
				wantPosts = 1
			}
			if h.posts() != wantPosts {
				t.Fatalf("%d approve(s) posted, want %d", h.posts(), wantPosts)
			}
		})
	}
}

func TestApproveOrderGrantBeforeScope(t *testing.T) {
	// A non-aether request with no grant names the grant: checks run in order.
	h, tm := world()
	f := good()
	f["repo"] = "bic/app-studio"
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(f)}, func(p *Pass) {
		p.Grants = func() ([]grants.Grant, error) { return nil, nil }
	})
	if r := out.Results["r1"]; !strings.HasPrefix(r.Reason, "grant:") || len(h.calls) != 0 {
		t.Fatalf("result %+v, gh calls %v", r, h.calls)
	}
}

func TestApproveDecidedNeverRerun(t *testing.T) {
	h, tm := world()
	f := good()
	f["result"] = map[string]any{"ok": false, "reason": "grant: x", "at": "t"}
	odd := good()
	odd["result"] = "garbage"
	out, err := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(f), "r2": reqJSON(odd), "r3": json.RawMessage(`"not an object"`)})
	if err != nil || len(out.Results) != 0 || len(h.calls) != 0 {
		t.Fatalf("decided requests re-run: %+v %v %v", out.Results, h.calls, err)
	}
}

func TestApproveAskUsedOnce(t *testing.T) {
	h, tm := world()
	first := good()
	first["result"] = map[string]any{"ok": true, "review_id": 77, "at": "t"}
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(first), "r2": reqJSON(good())})
	if r := out.Results["r2"]; r.OK || !strings.Contains(r.Reason, "already approved through request r1") || h.posts() != 0 {
		t.Fatalf("a used ask approved again: %+v", r)
	}
	// Two new requests on one ask in one round: the second is refused.
	h, tm = world()
	out, _ = run(t, h, tm, map[string]json.RawMessage{"a": reqJSON(good()), "b": reqJSON(good())})
	if !out.Results["a"].OK || out.Results["b"].OK || h.posts() != 1 {
		t.Fatalf("one ask, two approves: %+v posts %d", out.Results, h.posts())
	}
}

func TestApproveRecoversAPostWithoutResult(t *testing.T) {
	h, tm := world()
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 555, State: "APPROVED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 555 || h.posts() != 0 {
		t.Fatalf("recovery: %+v posts %d", r, h.posts())
	}
}

func TestApproveDryRunPostsNothing(t *testing.T) {
	h, tm := world()
	out, err := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good()), "r2": reqJSON(map[string]any{"repo": "bic/app-studio",
		"pr": 4009, "teams_msg_id": msgID, "chat": "ahmed"})}, func(p *Pass) { p.DryRun = true })
	if err != nil || len(out.Results) != 0 || len(out.Pings) != 0 || h.posts() != 0 || len(h.calls) == 0 {
		t.Fatalf("dry run: results %+v pings %+v posts %d calls %d err %v", out.Results, out.Pings, h.posts(), len(h.calls), err)
	}
	log := strings.Join(out.Log, "\n")
	if !strings.Contains(log, "dry run: request r1 would be prepared for the approve") || !strings.Contains(log, "r2 would be refused: scope: repo") {
		t.Fatalf("dry log: %s", log)
	}
	// And the real exec is never reached in dry mode: a GH whose exec is
	// the real one would fail the test binary's environment (no gh); the
	// recorder above saw every call and none was a post.
}

func TestApproveTransientLeavesNoResult(t *testing.T) {
	h, tm := world()
	h.readErr = errors.New("gh: connection reset")
	out, err := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if err == nil || len(out.Results) != 0 {
		t.Fatalf("a read failure became a result: %+v %v", out.Results, err)
	}
	h, tm = world()
	tm.err = errors.New("tomb timed out")
	out, err = run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if err == nil || len(out.Results) != 0 || h.posts() != 0 {
		t.Fatalf("a Teams failure became a result or a post: %+v %v", out.Results, err)
	}
}

func TestApproveCapsARound(t *testing.T) {
	h, tm := world()
	reqs := map[string]json.RawMessage{}
	for i := 0; i < MaxPerRound+2; i++ {
		f := good()
		f["repo"] = "bic/app-studio"
		reqs[fmt.Sprintf("r%d", i)] = reqJSON(f)
	}
	out, _ := run(t, h, tm, reqs)
	if len(out.Results) != MaxPerRound {
		t.Fatalf("%d decided, want %d", len(out.Results), MaxPerRound)
	}
	ids := make([]string, 0)
	for id := range out.Results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if ids[0] != "r0" || ids[MaxPerRound-1] != fmt.Sprintf("r%d", MaxPerRound-1) {
		t.Fatalf("not oldest id first: %v", ids)
	}
}

func TestNamesPR(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"approve #110001 please", true},
		{"approve #110001.", true},
		{"(#110001)", true},
		{"https://microsoft.ghe.com/bic/aether/pull/110001/files", true},
		{"#1100011", false},
		{"#11000", false},
		{"app-studio#110001", false},
		{"bic/aether#110001", false},
		{"https://microsoft.ghe.com/bic/aether/pull/1100011", false},
		{`<a href="https://microsoft.ghe.com/bic/app-studio/pull/110001">#110001</a>`, false},
		{"https://microsoft.ghe.com/bic/app-studio/pull/110001 and #110001", false},
		{"pr 110001", false},
	} {
		if got := namesPR(c.s, 110001); got != c.want {
			t.Errorf("namesPR(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestLiveGrantShape(t *testing.T) {
	// The live file's shape (band-grants save()): principal is the band-grants
	// principal name, never figaro's sandbox principal.
	b := []byte(`{"grants":[{"id":"c1","principal":"sbx-ops-sec","credential":"ebac-approve.aether","expires_at":1794532485,"approved_by":"jack"}]}`)
	p := t.TempDir() + "/grants.json"
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	gs, err := grants.ReadGrants(p)
	if err != nil || grants.LiveGrant(gs, "sbx-ops-sec", "ebac-approve.aether", 1794532484) != "" ||
		grants.LiveGrant(gs, "sandbox:ops-sec@local:box", "ebac-approve.aether", 1794532484) == "" {
		t.Fatalf("grants %+v %v", gs, err)
	}
	if _, err := grants.ReadGrants(t.TempDir() + "/none.json"); err == nil {
		t.Fatal("a missing grants file read as no error")
	}
}

// Only an APPROVED review carrying the body is ours: a comment quoting it is
// not a past approve, so the approve is still posted.
func TestApproveRecoveryNeedsAnApproval(t *testing.T) {
	h, tm := world()
	// A review carrying the ask's Body in a non-approve state: the ask is taken, never posted; a final refusal.
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 556, State: "COMMENTED", Body: Body(watch.Asker(), msgID), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; r.OK || r.Reason != ReadBackFailed+"review 556 carries the request's marker in state COMMENTED" || h.posts() != 0 {
		t.Fatalf("a comment with the ask's body: %+v posts %d", r, h.posts())
	}
	// The marker on a non-approve review: ours, never posted again, a final refusal (not ok).
	h, tm = world()
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 558, State: "COMMENTED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ = run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; r.OK || r.Reason != ReadBackFailed+"review 558 carries the request's marker in state COMMENTED" || h.posts() != 0 {
		t.Fatalf("marker on a comment: %+v posts %d", r, h.posts())
	}
}

// One inputs form, two askers: each critic decides only the requests naming
// its own chat alias and leaves the other's alone (no result, not counted),
// and its review body names its own asker.
func TestApproveTwoAskersOneForm(t *testing.T) {
	sid := good()
	sid["chat"], sid["teams_msg_id"], sid["pr"] = "siddhant", "1794516486157", 110001
	bad := good()
	bad["chat"] = 3
	reqs := map[string]json.RawMessage{"a1": reqJSON(good()), "s1": reqJSON(sid), "x1": reqJSON(bad)}

	h, tm := world()
	out, err := run(t, h, tm, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || !out.Results["a1"].OK {
		t.Fatalf("ahmed critic: results %+v (want a1 ok only; s1 and x1 untouched)", out.Results)
	}
	last := h.calls[len(h.calls)-2]
	if !strings.HasSuffix(strings.Join(last, " "), "on Pat Doe's ask "+msgID+".\n\n<!-- figaro-ask:a1 -->") || !strings.Contains(out.Pings[0].Text, "Pat Doe") {
		t.Fatalf("ahmed body %q ping %q", last, out.Pings[0].Text)
	}

	// The siddhant critic, same form: decides s1 only. Its ask check runs
	// against its own chat, which this fake world does not hold, so s1 is
	// refused at the ask step: that is a decision, made by the right critic.
	h2, tm2 := world()
	out2, err := run(t, h2, tm2, reqs, func(p *Pass) {
		p.Watch.ChatAlias, p.Watch.AskName = "siddhant", "Siddhant Lahoti"
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out2.Results["s1"]; !ok || len(out2.Results) != 1 {
		t.Fatalf("siddhant critic: results %+v (want s1 only)", out2.Results)
	}
	if got := Body("Siddhant Lahoti", "1"); got != "Approved on Jack's behalf (Figaro), on Siddhant Lahoti's ask 1." {
		t.Fatalf("body %q", got)
	}
	// Default asker: the alias.
	if (&model.ApproveWatch{ChatAlias: "siddhant"}).Asker() != "siddhant" {
		t.Fatal("default asker is not the alias")
	}
}

func TestCheckWatchAskName(t *testing.T) {
	w := watch
	if err := ghcfg.CheckApprove(&w); err != nil {
		t.Fatalf("good name refused: %v", err)
	}
	for _, bad := range []string{"Ahmed\nKotb", "<b>x</b>", "x`y`", "1abc"} {
		w.AskName = bad
		if err := ghcfg.CheckApprove(&w); err == nil || !strings.Contains(err.Error(), "ask name") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// Query is a read: a mutation never reaches gh; a read asks rateLimit and
// books its cost.
func TestQueryReadsOnly(t *testing.T) {
	var argv [][]string
	gh := &GH{exec: func(_ context.Context, args []string) ([]byte, error) {
		argv = append(argv, args)
		return []byte(`{"data":{"rateLimit":{"cost":2,"remaining":4000},"u0":{"nodes":[]}}}`), nil
	}}
	for _, q := range []string{`mutation{ addPullRequestReview(input:{}){clientMutationId} }`, `query{ a: x } mutation{ y }`, `{ viewer{login} }`} {
		if _, err := gh.Query(context.Background(), q); err == nil {
			t.Errorf("%q ran", q)
		}
	}
	if len(argv) != 0 {
		t.Fatalf("a refused query reached gh: %v", argv)
	}
	data, err := gh.Query(context.Background(), `query{ u0: search(type:ISSUE, first:1, query:"x"){nodes{__typename}} }`)
	if err != nil || strings.Contains(string(data), "rateLimit") || !strings.Contains(argv[0][3], "rateLimit{cost remaining}") {
		t.Fatalf("read: %s %v %v", data, err, argv)
	}
	if s := gh.Spent(); s.Calls != 1 || s.Points != 2 || s.Remaining != 4000 {
		t.Fatalf("spend %+v", s)
	}
}

// bic/aether#129969 under design B: a read-back error after the post is never a retry that could post again. The
// post round leaves no result; later rounds only read back; LostAfter (10 min) after the post, a final refusal and a
// ping. Exactly one POST throughout.
func TestReadBackErrorAfterThePostIsFinal(t *testing.T) {
	h, tm := world()
	h.readErrAfterPost = errors.New("gh: HTTP 502")
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	l := &ledger{}
	out, err := l.round(t, h, tm, reqs, now)
	if err != nil || len(out.Results) != 0 || h.posts() != 1 || l.posts["r1"].PostedAt != "2026-11-12T21:00:00Z" {
		t.Fatalf("post round: %+v err %v posts %d ledger %+v", out.Results, err, h.posts(), l.posts["r1"])
	}
	out, _ = l.round(t, h, tm, reqs, now.Add(LostAfter-time.Second))
	if len(out.Results) != 0 || h.posts() != 1 {
		t.Fatalf("inside LostAfter: %+v posts %d", out.Results, h.posts())
	}
	out, _ = l.round(t, h, tm, reqs, now.Add(LostAfter))
	r := out.Results["r1"]
	if r.OK || !strings.HasPrefix(r.Reason, ReadBackFailed+"posted at 2026-11-12T21:00:00Z, not read back within 10m0s (read-back failed: gh: HTTP 502)") ||
		h.posts() != 1 || len(out.Pings) != 1 || !strings.Contains(out.Pings[0].Text, "may have been SENT") {
		t.Fatalf("at LostAfter: %+v posts %d pings %+v", r, h.posts(), out.Pings)
	}
	// decided: the next round posts nothing and drops the record
	decided(reqs, out)
	h.readErrAfterPost = nil
	out, _ = l.round(t, h, tm, reqs, now.Add(LostAfter+time.Minute))
	if len(out.Results) != 0 || h.posts() != 1 || len(l.posts) != 0 {
		t.Fatalf("after the result: %+v posts %d ledger %v", out.Results, h.posts(), l.posts)
	}
}

// An approve on page 2 (150 reviews) is found: no second post.
func TestAnApproveOnPageTwoIsFound(t *testing.T) {
	h, tm := world()
	for i := 0; i < 150; i++ {
		h.reviews[110001] = append(h.reviews[110001], Review{ID: int64(100 + i), State: "COMMENTED", Body: "x", SubmittedAt: "2026-11-12T20:00:00Z"})
	}
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 777, State: "APPROVED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 777 || h.posts() != 0 {
		t.Fatalf("page 2: %+v posts %d", out.Results, h.posts())
	}
}

// Our approve DISMISSED by a push is still ours: a re-decided request does
// not post again.
func TestDismissedApproveCountsAsPosted(t *testing.T) {
	h, tm := world()
	// The original intent (bd08e58): our approve as the interim posted it (Body, no marker), dismissed by a push.
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 557, State: "DISMISSED", Body: Body(watch.Asker(), msgID), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 557 || h.posts() != 0 {
		t.Fatalf("dismissed approve: %+v posts %d", r, h.posts())
	}
}

// A request whose approve was SENT but not read back uses its ask: a
// re-filed request for the same Teams message is refused, never posted.
func TestSentButUnreadAskIsUsed(t *testing.T) {
	h, tm := world()
	f := good()
	f["result"] = map[string]any{"ok": false, "reason": ReadBackFailed + "gh: HTTP 502", "at": "2026-11-12T21:00:00Z"}
	reqs := map[string]json.RawMessage{"r1": reqJSON(f), "r2": reqJSON(good())}
	out, _ := run(t, h, tm, reqs)
	if r := out.Results["r2"]; r.OK || !strings.Contains(r.Reason, "already approved through request r1") || h.posts() != 0 {
		t.Fatalf("re-filed: %+v posts %d", r, h.posts())
	}
}
