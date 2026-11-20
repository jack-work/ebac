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
	"github.com/gluck/ebac/sdk/cfg/github"
	"github.com/gluck/ebac/sdk/grants"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
	"github.com/gluck/ebac/sdk/tomb"
)

const (
	// Repo is the only repository this source can approve in.
	Repo = "bic/aether"
	// AskWindow is how many of the chat's newest messages the ask is looked
	// for in.
	AskWindow = 50
	// MaxPerRound bounds the requests decided in one round.
	MaxPerRound = 3
)

// Body is the review body, citing the asker and the ask.
func Body(asker, msgID string) string {
	return fmt.Sprintf("Approved on Jack's behalf (Figaro), on %s's ask %s.", asker, msgID)
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
	if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(ghcfg.Host) + `/` + regexp.QuoteMeta(Repo) + `/pull/` + num + `\b`).MatchString(content) {
		return true
	}
	// Another repository's /pull/n link makes a bare #n ambiguous: refuse.
	if regexp.MustCompile(`(?i)/pull/` + num + `\b`).MatchString(content) {
		return false
	}
	text := tagRe.ReplaceAllString(content, " ")
	return regexp.MustCompile(`(^|[^A-Za-z0-9_./-])#` + num + `\b`).MatchString(text)
}

// Pass is one round's input. Op is the phase (#27 B): OpRound decides and
// PREPARES (records the approve under Token, posts nothing); OpCommit posts
// what this round prepared, after the core saved it. Posts is the ledger as
// the core saved it (runtime.approve_posts).
type Pass struct {
	Watch    model.ApproveWatch
	Requests map[string]json.RawMessage // inputs form key requests
	Grants   func() ([]grants.Grant, error)
	Now      time.Time
	DryRun   bool
	Op       plugin.Op
	Token    string
	Posts    map[string]*model.PostRecord
}

// Out is one round's output: the results to write, the pings, the log, the
// ledger to save, and whether to ask the core for OpCommit.
type Out struct {
	Results map[string]Result
	Pings   []plugin.Ping
	Log     []string
	Posts   map[string]*model.PostRecord
	Commit  bool
}

// LostAfter: a prepared or posted approve not read back this long after is a
// final refusal (it may have posted: the requester checks by hand).
const LostAfter = 10 * time.Minute

// Marker tags a request's approve (an HTML comment, invisible on GitHub;
// names only figaro): found in ANY review state, the approve was posted.
func Marker(id string) string { return "<!-- figaro-ask:" + id + " -->" }

// PostBody is the body posted for a request: Body, then its marker.
func PostBody(asker, msgID, id string) string { return Body(asker, msgID) + "\n\n" + Marker(id) }

// errTransient marks an upstream failure: the request is left without a
// result and decided again next round.
var errTransient = errors.New("transient")

// ReadBackFailed starts the reason of a request whose approve may have been
// sent but was not read back: final, never retried (a retry could post
// twice); the ask is used and the requester is pinged to check the PR by hand.
const ReadBackFailed = "post: approve sent (check the PR by hand; not posted again): " // unchanged text: interim results with it keep their ask used

// Run is one phase of a round (Pass.Op). A dry run reads everything and
// decides, but prepares and posts nothing and returns no results or pings.
func Run(ctx context.Context, gh *GH, teams Teams, p Pass) (Out, error) {
	out := Out{Results: map[string]Result{}, Posts: map[string]*model.PostRecord{}}
	for id, r := range p.Posts {
		c := *r
		out.Posts[id] = &c
	}
	if p.Op == plugin.OpCommit {
		return out, commit(ctx, gh, p, &out)
	}
	logf := func(f string, a ...any) { out.Log = append(out.Log, fmt.Sprintf(f, a...)) }
	ids := make([]string, 0, len(p.Requests))
	used := map[string]string{} // teams_msg_id -> request id whose approve was (or may have been) sent
	decided := map[string]bool{}
	for id, raw := range p.Requests {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			logf("request %q is not an object: skipped (no result can be written on it)", id)
			continue
		}
		var msg string
		_ = json.Unmarshal(obj["teams_msg_id"], &msg)
		if obj["result"] != nil {
			// Decided: never re-run, whatever the result's shape. An
			// approve that was sent (ok, or may have been) uses the ask.
			decided[id] = true
			var res Result
			if json.Unmarshal(obj["result"], &res) == nil && (res.OK || strings.HasPrefix(res.Reason, ReadBackFailed)) && msg != "" {
				used[msg] = id
			}
			continue
		}
		if out.Posts[id] != nil && msg != "" {
			used[msg] = id // prepared or posted: the ask is taken
		}
		// One inputs form serves several askers, one critic each: a
		// request names its asker's chat, and only that critic decides it.
		var chat string
		if json.Unmarshal(obj["chat"], &chat) != nil || chat != p.Watch.ChatAlias {
			continue
		}
		ids = append(ids, id)
	}
	// The ledger keeps a record until its request has a result (or is gone).
	for id := range out.Posts {
		if _, ok := p.Requests[id]; !ok || decided[id] {
			delete(out.Posts, id)
		}
	}
	sort.Strings(ids)
	at := p.Now.UTC().Format(time.RFC3339)
	var firstErr error
	acted := 0
	for _, id := range ids {
		if acted >= MaxPerRound {
			logf("%d request(s) wait for later rounds", len(ids)-acted)
			break
		}
		var res Result
		var final bool
		var err error
		if rec := out.Posts[id]; rec != nil {
			// A record exists: no check runs again; only the read-back decides.
			res, final, err = readBack(ctx, gh, p, id, rec)
		} else {
			res, final, err = decide(ctx, gh, teams, p, id, used, &out)
		}
		acted++
		if err != nil {
			logf("request %s: %v (decided again next round)", id, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !final {
			continue
		}
		res.At = at
		if p.DryRun {
			logf("dry run: request %s would be %s", id, describe(res))
			continue
		}
		record(&out, p, id, res, used)
	}
	if p.DryRun {
		out.Commit, out.Posts = false, p.Posts
	}
	return out, firstErr
}

// record writes a final result, its ping, and marks a sent ask used.
func record(out *Out, p Pass, id string, res Result, used map[string]string) {
	out.Results[id] = res
	out.Log = append(out.Log, fmt.Sprintf("request %s: %s", id, describe(res)))
	r, _ := parse(id, p.Requests[id])
	sent := res.OK || strings.HasPrefix(res.Reason, ReadBackFailed)
	if sent {
		used[r.MsgID] = id
	}
	if p.Watch.PingTo == "" {
		return
	}
	switch {
	case res.OK:
		out.Pings = append(out.Pings, plugin.Ping{To: p.Watch.PingTo, Text: fmt.Sprintf(
			"ebac approve: %s#%d approved, review %d, on %s's ask %s (request %s)", Repo, r.PR, res.ReviewID, p.Watch.Asker(), r.MsgID, id)})
	case sent:
		out.Pings = append(out.Pings, plugin.Ping{To: p.Watch.PingTo, Text: fmt.Sprintf(
			"ebac approve: %s#%d approve may have been SENT but was not read back (request %s): check the PR by hand; ebac will not post again", Repo, r.PR, id)})
	}
}

// commit is OpCommit: post ONLY the records this round prepared (their token
// is this round's: the core saved them before calling), mark each posted
// (also when the POST errs: it may have landed), and read back once, no
// sleep. Unseen: later rounds read back; nothing here posts twice.
func commit(ctx context.Context, gh *GH, p Pass, out *Out) error {
	if p.Token == "" || p.DryRun {
		return nil
	}
	ids := make([]string, 0, len(out.Posts))
	for id, rec := range out.Posts {
		if rec.Token == p.Token && rec.PostedAt == "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	used := map[string]string{}
	at := p.Now.UTC().Format(time.RFC3339)
	for _, id := range ids {
		rec := out.Posts[id]
		r, why := parse(id, p.Requests[id])
		if why != "" {
			out.Log = append(out.Log, fmt.Sprintf("request %s: prepared but unparseable now (%s): not posted", id, why))
			continue
		}
		perr := gh.Approve(ctx, rec.PR, PostBody(p.Watch.Asker(), r.MsgID, id))
		rec.PostedAt = at
		if perr != nil {
			out.Log = append(out.Log, fmt.Sprintf("request %s: post reported %s (may have landed; read back from now on)", id, oneLine(perr.Error())))
		}
		back, err := gh.Reviews(ctx, rec.PR)
		if err != nil {
			out.Log = append(out.Log, fmt.Sprintf("request %s: posted; read-back failed (%s): read back next round", id, oneLine(err.Error())))
			continue
		}
		if rv := ours(back, rec.Marker); rv != nil {
			res := found(rv)
			res.At = at
			record(out, p, id, res, used)
			continue
		}
		out.Log = append(out.Log, fmt.Sprintf("request %s: posted, not yet read back: read back next round", id))
	}
	return nil
}

// readBack decides a request that has a record: the marker in any review
// state settles it; unseen LostAfter after the post (or, never confirmed
// posted, after the prepare) is a final refusal. A record never posts here.
func readBack(ctx context.Context, gh *GH, p Pass, id string, rec *model.PostRecord) (Result, bool, error) {
	back, err := gh.Reviews(ctx, rec.PR)
	if err == nil {
		if rv := ours(back, rec.Marker); rv != nil {
			return found(rv), true, nil
		}
	}
	since, what := rec.PostedAt, "posted"
	if since == "" {
		since, what = rec.PreparedAt, "prepared (never confirmed posted)"
	}
	t, perr := time.Parse(time.RFC3339, since)
	if perr != nil || p.Now.Sub(t) >= LostAfter {
		why := "no review with its marker"
		if err != nil {
			why = "read-back failed: " + oneLine(err.Error())
		}
		return Result{Reason: fmt.Sprintf("%s%s at %s, not read back within %s (%s); ask again with a new message", ReadBackFailed, what, since, LostAfter, why)}, true, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("%w: read back: %v", errTransient, err)
	}
	return Result{}, false, nil
}

// found is the result of a review carrying the request's marker: an approve
// (APPROVED, or DISMISSED by a later push) is ok; the marker on any other
// state is ours but not an approve: final, never posted again.
func found(rv *Review) Result {
	if rv.State == "APPROVED" || rv.State == "DISMISSED" {
		return Result{OK: true, ReviewID: rv.ID}
	}
	return Result{Reason: fmt.Sprintf("%sreview %d carries the request's marker in state %s", ReadBackFailed, rv.ID, rv.State)}
}

func describe(r Result) string {
	if r.OK {
		return fmt.Sprintf("approved (review %d)", r.ReviewID)
	}
	return "refused: " + r.Reason
}

func refuse(reason string) (Result, bool, error) { return Result{Reason: reason}, true, nil }

// decide runs the checks in order. A failed check is a refusal (a final
// result); an upstream failure is an error (no result, retried). Every check
// passed: the approve is PREPARED (recorded under this round's token, Commit
// asked), never posted here.
func decide(ctx context.Context, gh *GH, teams Teams, p Pass, id string, used map[string]string, out *Out) (Result, bool, error) {
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
	if why := grants.LiveGrant(gs, w.Principal, w.Grant, p.Now.Unix()); why != "" {
		return refuse("grant: " + why)
	}
	// 2. scope: bic/aether exactly, open, not draft. The event is APPROVE by
	// construction (GH.Approve is the only write).
	if r.Repo != Repo {
		return refuse(fmt.Sprintf("scope: repo %q is not %s", r.Repo, Repo))
	}
	pr, err := gh.PR(ctx, r.PR)
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") || strings.Contains(err.Error(), "404") {
			return refuse(fmt.Sprintf("scope: %s#%d not found", Repo, r.PR))
		}
		return Result{}, false, fmt.Errorf("%w: read PR: %v", errTransient, err)
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
		return Result{}, false, fmt.Errorf("%w: read chat: %v", errTransient, err)
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
		return Result{}, false, fmt.Errorf("%w: read reviews: %v", errTransient, err)
	}
	if rv := ours(reviews, Marker(id)); rv != nil {
		// This request's approve is on GitHub with no record here: settle it.
		return found(rv), true, nil
	}
	if rv := forAsk(reviews, Body(w.Asker(), r.MsgID)); rv != nil {
		// An approve for this ASK under any request id (a re-filed request
		// after a push dismissed ours, or an interim approve with no marker),
		// in any state: the ask was served; never post for it again.
		return found(rv), true, nil
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
	// 4. prepare: the record, saved by the core before OpCommit posts it.
	if p.DryRun {
		out.Log = append(out.Log, fmt.Sprintf("dry run: request %s would be prepared for the approve", id))
		return Result{}, false, nil
	}
	if p.Token == "" {
		return Result{}, false, fmt.Errorf("%w: no commit token from the core: nothing prepared", errTransient)
	}
	out.Posts[id] = &model.PostRecord{PR: r.PR, Marker: Marker(id), Token: p.Token, PreparedAt: p.Now.UTC().Format(time.RFC3339)}
	out.Commit = true
	used[r.MsgID] = id
	out.Log = append(out.Log, fmt.Sprintf("request %s: checks passed; approve of %s#%d prepared (posted at commit)", id, Repo, r.PR))
	return Result{}, false, nil
}

// forAsk is a review whose body starts with the ask's Body, in ANY state:
// Body names the asker and the Teams message, so it is the ask's whatever
// request id carried it.
func forAsk(rs []Review, body string) *Review {
	for i := range rs {
		if strings.HasPrefix(strings.TrimSpace(rs[i].Body), body) {
			return &rs[i]
		}
	}
	return nil
}

// ours is the review carrying marker, in ANY state: an approve of ours that a
// push dismissed is still posted. A review merely quoting the body without
// the exact marker is not ours.
func ours(rs []Review, marker string) *Review {
	for i := range rs {
		if strings.Contains(rs[i].Body, marker) {
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
