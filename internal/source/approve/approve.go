// Package approve serves approve-requests (ebac-approve): a sandboxed
// principal that holds no GHE credential asks ebac to APPROVE a bic/aether
// pull request by writing requests.<id> on the critic's inputs form; ebac
// posts the review only when every check passes, in order, and writes
// requests.<id>.result (GO-20261112-secretary-ahmed).
//
// Every request field is untrusted: the requester can write anything on the
// inputs form, including a claim about who it is (by is ignored). The
// configuration (chat, sender, grant, principal, ping role) is on ebac's own
// role form, which the requester cannot write; the repo and the review event
// are constants here.
package approve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/tomb"
)

const (
	// Repo is the only repository this source can approve in.
	Repo = "bic/aether"
	// Host is the forge host of Repo.
	Host = "microsoft.ghe.com"
	// AskWindow is how many of the chat's newest messages the ask is looked
	// for in.
	AskWindow = 50
	// MaxPerRound bounds the requests decided in one round.
	MaxPerRound = 3
)

// Body is the review body, citing the ask.
func Body(msgID string) string {
	return fmt.Sprintf("Approved on Jack's behalf (Figaro), on Ahmed's ask %s.", msgID)
}

// Teams reads a chat's newest messages (tomb).
type Teams interface {
	Messages(ctx context.Context, conv string, limit int) ([]tomb.TeamsMsg, error)
}

// Result is requests.<id>.result.
type Result struct {
	OK       bool   `json:"ok"`
	ReviewID int64  `json:"review_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
	At       string `json:"at"`
}

// request is one parsed request.
type request struct {
	ID    string
	Repo  string
	PR    int
	MsgID string
	Chat  string
}

var (
	idRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	msgIDRe = regexp.MustCompile(`^[0-9]{10,16}$`)
	fields  = map[string]bool{"repo": true, "pr": true, "teams_msg_id": true, "chat": true, "at": true, "by": true, "result": true}
	tagRe   = regexp.MustCompile(`<[^>]*>`)
)

// parse is the shape check: the reason it fails, or "".
func parse(id string, raw json.RawMessage) (request, string) {
	r := request{ID: id}
	if !idRe.MatchString(id) {
		return r, "shape: request id is not [A-Za-z0-9._-]{1,64}"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return r, "shape: request is not an object"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !fields[k] {
			return r, fmt.Sprintf("shape: unknown field %q", k)
		}
	}
	if json.Unmarshal(m["repo"], &r.Repo) != nil || r.Repo == "" {
		return r, "shape: repo must be a string"
	}
	// A bare JSON number: json.Number alone would also take "110001".
	praw := strings.TrimSpace(string(m["pr"]))
	var pr json.Number
	if praw == "" || praw[0] == '"' || json.Unmarshal([]byte(praw), &pr) != nil {
		return r, "shape: pr must be a number"
	}
	n, err := strconv.ParseInt(pr.String(), 10, 64)
	if err != nil || n < 1 || n > 10_000_000 {
		return r, "shape: pr must be an integer 1..10000000"
	}
	r.PR = int(n)
	if json.Unmarshal(m["teams_msg_id"], &r.MsgID) != nil || !msgIDRe.MatchString(r.MsgID) {
		return r, "shape: teams_msg_id must be a string of 10-16 digits"
	}
	if json.Unmarshal(m["chat"], &r.Chat) != nil {
		return r, "shape: chat must be a string"
	}
	return r, ""
}

// namesPR reports whether a Teams message names bic/aether pull request n:
// its URL, or #n standing alone (not owner/repo#n or another repo's link).
func namesPR(content string, n int) bool {
	num := strconv.Itoa(n)
	if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(Host) + `/` + regexp.QuoteMeta(Repo) + `/pull/` + num + `\b`).MatchString(content) {
		return true
	}
	// Another repository's /pull/n link makes a bare #n ambiguous: refuse.
	if regexp.MustCompile(`(?i)/pull/` + num + `\b`).MatchString(content) {
		return false
	}
	text := tagRe.ReplaceAllString(content, " ")
	return regexp.MustCompile(`(^|[^A-Za-z0-9_./-])#` + num + `\b`).MatchString(text)
}

// Pass is one round's input.
type Pass struct {
	Watch    model.ApproveWatch
	Requests map[string]json.RawMessage // inputs form key requests
	Grants   func() ([]Grant, error)
	Now      time.Time
	DryRun   bool
}

// Out is one round's output: the results to write, the pings, the log.
type Out struct {
	Results map[string]Result
	Pings   []plugin.Ping
	Log     []string
}

// errTransient marks an upstream failure: the request is left without a
// result and decided again next round.
var errTransient = errors.New("transient")

// Run decides up to MaxPerRound requests without a result, oldest id first.
// A dry run reads everything and decides, but posts nothing and returns no
// results or pings: only its log says what it would do.
func Run(ctx context.Context, gh *GH, teams Teams, p Pass) (Out, error) {
	out := Out{Results: map[string]Result{}}
	logf := func(f string, a ...any) { out.Log = append(out.Log, fmt.Sprintf(f, a...)) }
	ids := make([]string, 0, len(p.Requests))
	used := map[string]string{} // teams_msg_id -> request id of an ok result
	for id, raw := range p.Requests {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			logf("request %q is not an object: skipped (no result can be written on it)", id)
			continue
		}
		if obj["result"] != nil {
			// Decided: never re-run, whatever the result's shape. An ok
			// result marks its ask used.
			var res Result
			var msg string
			if json.Unmarshal(obj["result"], &res) == nil && res.OK && json.Unmarshal(obj["teams_msg_id"], &msg) == nil {
				used[msg] = id
			}
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	at := p.Now.UTC().Format(time.RFC3339)
	var firstErr error
	for _, id := range ids {
		if len(out.Results) >= MaxPerRound {
			logf("%d request(s) wait for later rounds", len(ids)-len(out.Results))
			break
		}
		res, err := decide(ctx, gh, teams, p, id, used)
		if err != nil {
			logf("request %s: %v (decided again next round)", id, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res.At = at
		if p.DryRun {
			logf("dry run: request %s would be %s", id, describe(res))
			continue
		}
		out.Results[id] = res
		logf("request %s: %s", id, describe(res))
		if res.OK {
			r, _ := parse(id, p.Requests[id])
			used[r.MsgID] = id
			if p.Watch.PingTo != "" {
				out.Pings = append(out.Pings, plugin.Ping{To: p.Watch.PingTo, Text: fmt.Sprintf(
					"ebac approve: %s#%d approved, review %d, on Ahmed's ask %s (request %s)", Repo, r.PR, res.ReviewID, r.MsgID, id)})
			}
		}
	}
	return out, firstErr
}

func describe(r Result) string {
	if r.OK {
		return fmt.Sprintf("approved (review %d)", r.ReviewID)
	}
	return "refused: " + r.Reason
}

func refuse(reason string) (Result, error) { return Result{Reason: reason}, nil }

// decide runs the checks in order. A failed check is a refusal (a final
// result); an upstream failure is an error (no result, retried).
func decide(ctx context.Context, gh *GH, teams Teams, p Pass, id string, used map[string]string) (Result, error) {
	w := p.Watch
	// 0. shape: every field is untrusted.
	r, why := parse(id, p.Requests[id])
	if why != "" {
		return refuse(why)
	}
	// 1. grant: exactly w.Grant for exactly w.Principal, live now.
	gs, err := p.Grants()
	if err != nil {
		return refuse("grant: grants unreadable: " + oneLine(err.Error()))
	}
	if why := liveGrant(gs, w.Principal, w.Grant, p.Now.Unix()); why != "" {
		return refuse("grant: " + why)
	}
	// 2. scope: bic/aether exactly, open, not draft. The event is APPROVE by
	// construction (GH.Approve is the only write).
	if r.Repo != Repo {
		return refuse(fmt.Sprintf("scope: repo %q is not %s", r.Repo, Repo))
	}
	if r.Chat != w.ChatAlias {
		return refuse(fmt.Sprintf("scope: chat %q is not %q", r.Chat, w.ChatAlias))
	}
	pr, err := gh.PR(ctx, r.PR)
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") || strings.Contains(err.Error(), "404") {
			return refuse(fmt.Sprintf("scope: %s#%d not found", Repo, r.PR))
		}
		return Result{}, fmt.Errorf("%w: read PR: %v", errTransient, err)
	}
	if pr.State != "open" {
		return refuse(fmt.Sprintf("scope: %s#%d is %s, not open", Repo, r.PR, pr.State))
	}
	if pr.Draft {
		return refuse(fmt.Sprintf("scope: %s#%d is a draft", Repo, r.PR))
	}
	// 3. ask: in the chat, from the asker, naming this PR, not used before,
	// newer than the PR's last approval (server timestamps, both UTC).
	msgs, err := teams.Messages(ctx, w.Chat, AskWindow)
	if err != nil {
		return Result{}, fmt.Errorf("%w: read chat: %v", errTransient, err)
	}
	var ask *tomb.TeamsMsg
	for i := range msgs {
		if msgs[i].ID == r.MsgID {
			ask = &msgs[i]
			break
		}
	}
	switch {
	case ask == nil:
		return refuse(fmt.Sprintf("ask: message %s is not among the chat's last %d", r.MsgID, AskWindow))
	case !ask.IsMessage() || ask.Deleted():
		return refuse(fmt.Sprintf("ask: message %s is not a live message", r.MsgID))
	case ask.SenderID() != w.AskFrom:
		return refuse(fmt.Sprintf("ask: message %s is from %s, not the asker", r.MsgID, ask.SenderID()))
	case !namesPR(ask.Content, r.PR):
		return refuse(fmt.Sprintf("ask: message %s does not name %s#%d", r.MsgID, Repo, r.PR))
	}
	if prev, ok := used[r.MsgID]; ok {
		return refuse(fmt.Sprintf("ask: message %s already approved through request %s", r.MsgID, prev))
	}
	askAt, err := time.Parse(time.RFC3339Nano, ask.Arrival)
	if err != nil {
		return refuse(fmt.Sprintf("ask: message %s has no server arrival time", r.MsgID))
	}
	reviews, err := gh.Reviews(ctx, r.PR)
	if err != nil {
		return Result{}, fmt.Errorf("%w: read reviews: %v", errTransient, err)
	}
	body := Body(r.MsgID)
	if rv := ours(reviews, body); rv != nil {
		// Posted in an earlier round that never wrote its result: record it.
		return Result{OK: true, ReviewID: rv.ID}, nil
	}
	for _, rv := range reviews {
		if rv.State != "APPROVED" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, rv.SubmittedAt)
		if err != nil {
			return refuse(fmt.Sprintf("ask: review %d has no submitted time", rv.ID))
		}
		if !askAt.UTC().After(t.UTC()) {
			return refuse(fmt.Sprintf("ask: message %s (%s) is not newer than approval %d (%s)",
				r.MsgID, askAt.UTC().Format(time.RFC3339), rv.ID, t.UTC().Format(time.RFC3339)))
		}
	}
	// 4. post, then read the review back.
	if p.DryRun {
		return Result{OK: true}, nil
	}
	perr := gh.Approve(ctx, r.PR, body)
	back, err := gh.Reviews(ctx, r.PR)
	if err != nil {
		return Result{}, fmt.Errorf("%w: read back: %v", errTransient, err)
	}
	if rv := ours(back, body); rv != nil {
		return Result{OK: true, ReviewID: rv.ID}, nil
	}
	if perr != nil {
		return refuse("post: " + oneLine(perr.Error()))
	}
	return refuse("post: approve sent, but no APPROVED review with its body read back")
}

// ours is the APPROVED review carrying body, if any.
func ours(rs []Review, body string) *Review {
	for i := range rs {
		if rs[i].State == "APPROVED" && strings.TrimSpace(rs[i].Body) == body {
			return &rs[i]
		}
	}
	return nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
