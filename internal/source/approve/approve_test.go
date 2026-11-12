package approve

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/tomb"
)

// Every argv any test in this package hands gh, checked in TestMain: the
// source can reach only these three shapes, whatever the requests say.
var (
	argvMu  sync.Mutex
	argvAll [][]string
	allowed = []*regexp.Regexp{
		regexp.MustCompile(`^api repos/bic/aether/pulls/[0-9]+$`),
		regexp.MustCompile(`^api repos/bic/aether/pulls/[0-9]+/reviews\?per_page=100$`),
		regexp.MustCompile(`^pr review [0-9]+ --repo bic/aether --approve --body Approved on Jack's behalf \(Figaro\), on Ahmed's ask [0-9]{10,16}\.$`),
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
	Principal: "sbx-cheroot-sec", PingTo: "@d407b690"}

var now = time.Date(2026, 11, 12, 21, 0, 0, 0, time.UTC)

// fakeHub is the fake GitHub: PRs and their reviews, answering by argv.
type fakeHub struct {
	prs      map[int]PRInfo
	reviews  map[int][]Review
	readErr  error // a non-404 read failure
	postErr  error // Approve fails
	postLost bool  // Approve "succeeds" but no review appears
	calls    [][]string
	nextID   int64
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
		path := a[1]
		n, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(path, "repos/bic/aether/pulls/"), "/")[0])
		if strings.Contains(path, "/reviews") {
			rs := h.reviews[n]
			if rs == nil {
				rs = []Review{}
			}
			return json.Marshal(rs)
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

func liveGrants() ([]Grant, error) {
	return []Grant{
		{ID: "a", Principal: "sbx-cheroot-sec", Credential: "tomb-live.notes", ExpiresAt: now.Unix() + 3600, ApprovedBy: "flake:cheroot"},
		{ID: "b", Principal: "sbx-cheroot-sec", Credential: "ebac-approve.aether", ExpiresAt: now.Unix() + 86400, ApprovedBy: "jack"},
	}, nil
}

// world is one scenario: an open aether PR 110001 with an old approval, and
// Ahmed's ask naming it after that approval.
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

func run(t *testing.T, h *fakeHub, tm fakeTeams, reqs map[string]json.RawMessage, mut ...func(*Pass)) (Out, error) {
	t.Helper()
	p := Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: now}
	for _, m := range mut {
		m(&p)
	}
	return Run(context.Background(), h.gh(), tm, p)
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
	if len(out.Pings) != 1 || out.Pings[0].To != "@d407b690" || !strings.Contains(out.Pings[0].Text, "bic/aether#110001") ||
		!strings.Contains(out.Pings[0].Text, msgID) || strings.ContainsAny(out.Pings[0].Text, "\n\r") {
		t.Fatalf("pings %+v", out.Pings)
	}
	last := h.calls[len(h.calls)-2]
	if strings.Join(last, " ") != "pr review 110001 --repo bic/aether --approve --body "+Body(msgID) {
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
	grants := func(gs ...Grant) func(*Pass) {
		return func(p *Pass) { p.Grants = func() ([]Grant, error) { return gs, nil } }
	}
	g := func(principal, cred string, exp int64, by string) Grant {
		return Grant{ID: "x", Principal: principal, Credential: cred, ExpiresAt: exp, ApprovedBy: by}
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
		{name: "chat not string", req: with("chat", 3), want: "shape: chat must be a string"},
		{name: "grants unreadable", req: reqJSON(good()), mut: func(p *Pass) {
			p.Grants = func() ([]Grant, error) { return nil, errors.New("open /var/lib/band-grants/grants.json: no such file") }
		}, want: "grant: grants unreadable"},
		{name: "grant absent", req: reqJSON(good()), mut: grants(), want: "grant: no live grant ebac-approve.aether for sbx-cheroot-sec"},
		{name: "grant figaro principal", req: reqJSON(good()), mut: grants(g("sandbox:cheroot-sec@local:box", "ebac-approve.aether", live, "jack")),
			want: "grant: no live grant"},
		{name: "grant prefix only", req: reqJSON(good()), mut: grants(g("sbx-cheroot-sec", "ebac-approve.*", live, "jack")), want: "grant: no live grant"},
		{name: "grant longer name", req: reqJSON(good()), mut: grants(g("sbx-cheroot-sec", "ebac-approve.aether2", live, "jack")), want: "grant: no live grant"},
		{name: "grant other principal", req: reqJSON(good()), mut: grants(g("sbx-other", "ebac-approve.aether", live, "jack")), want: "grant: no live grant"},
		{name: "grant expired", req: reqJSON(good()), mut: grants(g("sbx-cheroot-sec", "ebac-approve.aether", now.Unix(), "jack")), want: "expired"},
		{name: "grant standing", req: reqJSON(good()), mut: grants(g("sbx-cheroot-sec", "ebac-approve.aether", live, "flake:cheroot")), want: "is standing"},
		{name: "grant no approver", req: reqJSON(good()), mut: grants(g("sbx-cheroot-sec", "ebac-approve.aether", live, "")), want: "is standing"},
		{name: "repo app-studio", req: with("repo", "bic/app-studio"), want: `scope: repo "bic/app-studio" is not bic/aether`},
		{name: "repo case", req: with("repo", "bic/Aether"), want: "scope: repo"},
		{name: "chat alias", req: with("chat", "siddhant"), want: `scope: chat "siddhant"`},
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
		{name: "post fails", req: reqJSON(good()), hub: func(h *fakeHub) { h.postErr = errors.New("gh: Can not approve your own pull request") },
			want: "post: gh: Can not approve your own pull request"},
		{name: "post lost", req: reqJSON(good()), hub: func(h *fakeHub) { h.postLost = true }, want: "post: approve sent, but no APPROVED review"},
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
			if len(out.Pings) != 0 {
				t.Fatalf("a refusal pinged: %+v", out.Pings)
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
		p.Grants = func() ([]Grant, error) { return nil, nil }
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
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 555, State: "APPROVED", Body: Body(msgID), SubmittedAt: "2026-11-12T20:55:00Z"})
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
	if !strings.Contains(log, "r1 would be approved") || !strings.Contains(log, "r2 would be refused: scope: repo") {
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
	b := []byte(`{"grants":[{"id":"c1","principal":"sbx-cheroot-sec","credential":"ebac-approve.aether","expires_at":1794532485,"approved_by":"jack"}]}`)
	p := t.TempDir() + "/grants.json"
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	gs, err := ReadGrants(p)
	if err != nil || liveGrant(gs, "sbx-cheroot-sec", "ebac-approve.aether", 1794532484) != "" ||
		liveGrant(gs, "sandbox:cheroot-sec@local:box", "ebac-approve.aether", 1794532484) == "" {
		t.Fatalf("grants %+v %v", gs, err)
	}
	if _, err := ReadGrants(t.TempDir() + "/none.json"); err == nil {
		t.Fatal("a missing grants file read as no error")
	}
}

// Only an APPROVED review carrying the body is ours: a comment quoting it is
// not a past approve, so the approve is still posted.
func TestApproveRecoveryNeedsAnApproval(t *testing.T) {
	h, tm := world()
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 556, State: "COMMENTED", Body: Body(msgID), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; !r.OK || r.ReviewID == 556 || h.posts() != 1 {
		t.Fatalf("a comment counted as the approve: %+v posts %d", r, h.posts())
	}
}
