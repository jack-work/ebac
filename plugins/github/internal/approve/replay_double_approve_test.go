package approve

// Replay of the 2026-11-20 double approve (two APPROVE posts 22 s apart, 04:42:35Z and 04:42:57Z), from the
// incident's own inputs with every identifier synthetic (testing standard 2.6). It is written against the API every version of this package
// shares (GH's exec, Run, Pass, Out.Results); the two-phase fields of #27 design B (Pass.Op, Token, Posts;
// Out.Commit, Posts) are driven by reflection when present. That way the same file runs on the pre-fix approve path (expected red:
// two POSTs) and at B (green: one).
//
// test-data: testdata/replay-double-approve/reviews-before.json holds the 105 reviews on the PR before the first post, as
// GET .../pulls/<n>/reviews returned them on 2026-11-20 (ids, shas, logins and bodies synthetic). There were more than one
// page's worth, so the approve lands at index 105, on page 2.
// test-data: testdata/replay-double-approve/incident.json holds the request, the ask and the two posts.
//
// Observed: the two APPROVE reviews by our account, 22 s apart, and 105 earlier reviews.
// Inferred (the incident's trigger, not logged): the first round's read-back after the POST failed. On the pre-fix approve
// path a read-back error was transient, so the next round decided again; that path also read one page, so the new review on
// page 2 was unseen and the round posted again.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/grants"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/tomb"
)

//go:embed testdata/replay-double-approve/reviews-before.json
var replayReviews []byte

//go:embed testdata/replay-double-approve/incident.json
var replayIncident []byte

type replayWorld struct {
	reviews  []json.RawMessage
	posts    []string // POST bodies
	failNext bool     // the next read after a POST fails once (the inferred trigger)
	at       time.Time
}

func (w *replayWorld) gh() *GH {
	return &GH{exec: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "pr" {
			body := args[len(args)-1]
			w.posts = append(w.posts, body)
			rv, _ := json.Marshal(map[string]any{"id": len(w.posts), "state": "APPROVED", "body": body,
				"submitted_at": w.at.Format(time.RFC3339), "commit_id": strings.Repeat("a", 40),
				"user": map[string]any{"login": "self", "type": "User"}})
			w.reviews = append(w.reviews, rv)
			w.failNext = len(w.posts) == 1
			return nil, nil
		}
		path := args[1]
		if strings.HasSuffix(path, "/pulls/4242") {
			return []byte(`{"state":"open","draft":false,"head":{"sha":"` + strings.Repeat("a", 40) + `"}}`), nil
		}
		if w.failNext {
			w.failNext = false
			return nil, errors.New("gh api: HTTP 502")
		}
		page := 1
		if i := strings.Index(path, "&page="); i >= 0 {
			page, _ = strconv.Atoi(path[i+6:])
		}
		lo, hi := min((page-1)*100, len(w.reviews)), min(page*100, len(w.reviews))
		return json.Marshal(w.reviews[lo:hi])
	}}
}

type replayTeams struct{ msgs []tomb.TeamsMsg }

func (r replayTeams) Messages(context.Context, string, int) ([]tomb.TeamsMsg, error) {
	return r.msgs, nil
}

// The incident replayed: one request, rounds every 22 s, the first read-back after the POST failing once. Exactly one
// APPROVE may be posted and the request must end with an ok result. RED on the pre-fix approve path (two POSTs, 22 s apart, as on
// 2026-11-20); GREEN at #27 design B.
func TestReplayDoubleApprove(t *testing.T) {
	var inc struct {
		PR        int    `json:"pr"`
		RequestID string `json:"request_id"`
		MsgID     string `json:"teams_msg_id"`
		Arrival   string `json:"ask_arrival"`
		Before    int    `json:"reviews_before"`
	}
	if err := json.Unmarshal(replayIncident, &inc); err != nil {
		t.Fatal(err)
	}
	w := &replayWorld{}
	if err := json.Unmarshal(replayReviews, &w.reviews); err != nil {
		t.Fatal(err)
	}
	if len(w.reviews) != inc.Before || inc.Before <= 100 {
		t.Fatalf("fixture: %d reviews, incident says %d (must exceed one page)", len(w.reviews), inc.Before)
	}
	asker := "8:orgid:00000000-0000-0000-0000-00000000000a"
	chat := "19:replay@unq.gbl.spaces"
	watch := model.ApproveWatch{Chat: chat, ChatAlias: "asker", AskFrom: asker, Grant: "ebac-approve.aether",
		Principal: "replay-principal", PingTo: "@0000000a", AskName: "Asker A"}
	teams := replayTeams{msgs: []tomb.TeamsMsg{{ID: inc.MsgID, MessageType: "RichText/Html",
		FromURL: "https://x/v1/users/ME/contacts/" + asker, Content: "please approve #4242",
		Arrival: inc.Arrival}}}
	start := time.Date(2026, 11, 20, 4, 42, 35, 0, time.UTC)
	gs := func() ([]grants.Grant, error) {
		return []grants.Grant{{ID: "g", Principal: "replay-principal", Credential: "ebac-approve.aether", ExpiresAt: start.Add(24 * time.Hour).Unix(), ApprovedBy: "jack"}}, nil
	}
	req := map[string]any{"repo": Repo, "pr": inc.PR, "teams_msg_id": inc.MsgID, "chat": "asker", "at": "2026-11-20T04:42:00Z", "by": "x"}
	var ledger reflect.Value // Out.Posts of design B, carried as the core saves it
	var result *Result
	for round := 0; round < 4 && result == nil; round++ {
		w.at = start.Add(time.Duration(round) * 22 * time.Second)
		raw, _ := json.Marshal(req)
		p := Pass{Watch: watch, Requests: map[string]json.RawMessage{inc.RequestID: raw}, Grants: gs, Now: w.at}
		pv := reflect.ValueOf(&p).Elem()
		twoPhase := pv.FieldByName("Op").IsValid()
		if twoPhase {
			pv.FieldByName("Op").SetString("round")
			pv.FieldByName("Token").SetString(strconv.FormatInt(int64(round)+0x1000000000000000, 16))
			if ledger.IsValid() {
				pv.FieldByName("Posts").Set(ledger)
			}
		}
		out, _ := Run(context.Background(), w.gh(), teams, p)
		if r, ok := out.Results[inc.RequestID]; ok {
			result = &r
			break
		}
		if !twoPhase {
			continue
		}
		ov := reflect.ValueOf(&out).Elem()
		ledger = ov.FieldByName("Posts")
		if !ov.FieldByName("Commit").Bool() {
			continue
		}
		pv.FieldByName("Op").SetString("commit")
		pv.FieldByName("Posts").Set(ledger)
		c, _ := Run(context.Background(), w.gh(), teams, p)
		ledger = reflect.ValueOf(&c).Elem().FieldByName("Posts")
		if r, ok := c.Results[inc.RequestID]; ok {
			result = &r
		}
	}
	if len(w.posts) != 1 {
		t.Fatalf("double-approve replay: %d APPROVE posts (the incident's double approve), want exactly 1", len(w.posts))
	}
	if result == nil || !result.OK {
		t.Fatalf("double-approve replay: request ended %+v, want an ok result", result)
	}
}
