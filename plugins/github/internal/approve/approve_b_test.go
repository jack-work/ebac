package approve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/grants"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// #27 B: OpRound prepares and posts nothing; the record carries this round's token. RED if the post moves back into
// OpRound or the record is dropped (mutant "drop the record").
func TestRoundPreparesAndPostsNothing(t *testing.T) {
	h, tm := world()
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	out, err := Run(context.Background(), h.gh(), tm, Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: now, Op: plugin.OpRound, Token: "00000000000000aa"})
	if err != nil || h.posts() != 0 || !out.Commit || len(out.Results) != 0 {
		t.Fatalf("prepare: err %v posts %d commit %v results %+v", err, h.posts(), out.Commit, out.Results)
	}
	if rec := out.Posts["r1"]; rec == nil || *rec != (model.PostRecord{PR: 110001, Marker: "<!-- figaro-ask:r1 -->", Token: "00000000000000aa", PreparedAt: "2026-11-12T21:00:00Z"}) {
		t.Fatalf("record %+v", out.Posts["r1"])
	}
	// No token from the core: nothing prepared, a transient error.
	h, tm = world()
	out, err = Run(context.Background(), h.gh(), tm, Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: now, Op: plugin.OpRound})
	if err == nil || out.Commit || len(out.Posts) != 0 || h.posts() != 0 {
		t.Fatalf("no token: err %v commit %v posts %v", err, out.Commit, out.Posts)
	}
}

// A commit posts only records prepared under ITS token: a foreign token (another round's) never posts. RED if commit
// ignores the token (mutant).
func TestCommitWithAForeignTokenPostsNothing(t *testing.T) {
	h, tm := world()
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	posts := map[string]*model.PostRecord{"r1": {PR: 110001, Marker: Marker("r1"), Token: "00000000000000aa", PreparedAt: "2026-11-12T21:00:00Z"}}
	out, err := Run(context.Background(), h.gh(), tm, Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: now, Op: plugin.OpCommit, Token: "00000000000000bb", Posts: posts})
	if err != nil || h.posts() != 0 || out.Posts["r1"].PostedAt != "" {
		t.Fatalf("foreign token: err %v posts %d record %+v", err, h.posts(), out.Posts["r1"])
	}
	// The same record under its own token posts once.
	out, _ = Run(context.Background(), h.gh(), tm, Pass{Watch: watch, Requests: reqs, Grants: liveGrants, Now: now, Op: plugin.OpCommit, Token: "00000000000000aa", Posts: posts})
	if h.posts() != 1 || out.Posts["r1"].PostedAt != "2026-11-12T21:00:00Z" || !out.Results["r1"].OK {
		t.Fatalf("own token: posts %d record %+v result %+v", h.posts(), out.Posts["r1"], out.Results["r1"])
	}
}

// The core crashed between the save and the commit: the next round (a new token) never posts the prepared record;
// LostAfter after the prepare, a final refusal ("may have posted"). Zero POSTs.
func TestACrashBetweenPrepareAndPostNeverPosts(t *testing.T) {
	h, tm := world()
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	l := &ledger{crash: true}
	if _, err := l.round(t, h, tm, reqs, now); err != nil || l.posts["r1"] == nil || h.posts() != 0 {
		t.Fatalf("prepare: %v ledger %v posts %d", err, l.posts, h.posts())
	}
	l.crash = false
	out, _ := l.round(t, h, tm, reqs, now.Add(LostAfter-time.Second))
	if len(out.Results) != 0 || h.posts() != 0 {
		t.Fatalf("next round: %+v posts %d", out.Results, h.posts())
	}
	out, _ = l.round(t, h, tm, reqs, now.Add(LostAfter))
	r := out.Results["r1"]
	if r.OK || !strings.HasPrefix(r.Reason, ReadBackFailed+"prepared (never confirmed posted) at 2026-11-12T21:00:00Z") || h.posts() != 0 || len(out.Pings) != 1 {
		t.Fatalf("at LostAfter: %+v posts %d pings %v", r, h.posts(), out.Pings)
	}
}

// The POST landed but the commit reply (posted_at) was lost: the record still reads unposted with the OLD token. The
// next round never re-posts (its token differs); the review, visible late, settles it. Exactly one POST. RED if a
// record whose token passed is posted again.
func TestALostCommitReplyNeverReposts(t *testing.T) {
	h, tm := world()
	h.postLost = true // the POST lands but is not visible yet
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	l := &ledger{lose: true}
	out, _ := l.round(t, h, tm, reqs, now)
	if h.posts() != 1 || len(out.Results) != 0 || l.posts["r1"].PostedAt != "" {
		t.Fatalf("post round: posts %d results %+v ledger %+v", h.posts(), out.Results, l.posts["r1"])
	}
	l.lose = false
	out, _ = l.round(t, h, tm, reqs, now.Add(time.Minute))
	if h.posts() != 1 || len(out.Results) != 0 {
		t.Fatalf("next round: posts %d results %+v", h.posts(), out.Results)
	}
	// The review becomes visible (GHE lag): exactly one review, result ok.
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 4242, State: "APPROVED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T21:00:01Z"})
	out, _ = l.round(t, h, tm, reqs, now.Add(2*time.Minute))
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 4242 || h.posts() != 1 {
		t.Fatalf("late visibility: %+v posts %d", r, h.posts())
	}
}

// A post whose review is not visible at once (lag, posted_at saved): later rounds read back only; when it shows,
// exactly one review and result ok.
func TestALostPostThenLateVisibilityIsOneReview(t *testing.T) {
	h, tm := world()
	h.postLost = true
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	l := &ledger{}
	out, _ := l.round(t, h, tm, reqs, now)
	if h.posts() != 1 || len(out.Results) != 0 || l.posts["r1"].PostedAt == "" {
		t.Fatalf("post round: posts %d results %+v", h.posts(), out.Results)
	}
	for i := 1; i <= 3; i++ {
		if out, _ = l.round(t, h, tm, reqs, now.Add(time.Duration(i)*time.Minute)); len(out.Results) != 0 || h.posts() != 1 {
			t.Fatalf("round %d: %+v posts %d", i, out.Results, h.posts())
		}
	}
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 4243, State: "APPROVED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T21:00:01Z"})
	out, _ = l.round(t, h, tm, reqs, now.Add(4*time.Minute))
	n := 0
	for _, rv := range h.reviews[110001] {
		if strings.Contains(rv.Body, Marker("r1")) {
			n++
		}
	}
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 4243 || h.posts() != 1 || n != 1 {
		t.Fatalf("visible: %+v posts %d reviews with the marker %d", r, h.posts(), n)
	}
}

// Once a record exists, no check runs again: the PR closed and the grant gone, the read-back alone decides (ok).
// RED if the checks re-run after the record (a refusal for a request whose approve is on GitHub).
func TestChecksDoNotRerunAfterTheRecord(t *testing.T) {
	h, tm := world()
	h.postLost = true
	reqs := map[string]json.RawMessage{"r1": reqJSON(good())}
	l := &ledger{}
	l.round(t, h, tm, reqs, now)
	h.prs[110001] = PRInfo{State: "closed"}
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 4244, State: "DISMISSED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T21:00:01Z"})
	out, _ := l.round(t, h, tm, reqs, now.Add(time.Minute), func(p *Pass) { p.Grants = noGrants })
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 4244 || h.posts() != 1 {
		t.Fatalf("after the record: %+v posts %d", r, h.posts())
	}
}

// A dismissed approve carrying the marker on page 2 of 150 reviews: posted, no second post (B2 + all pages).
func TestADismissedMarkerOnPageTwoCountsAsPosted(t *testing.T) {
	h, tm := world()
	for i := 0; i < 150; i++ {
		h.reviews[110001] = append(h.reviews[110001], Review{ID: int64(100 + i), State: "COMMENTED", Body: "x", SubmittedAt: "2026-11-12T20:00:00Z"})
	}
	h.reviews[110001] = append(h.reviews[110001], Review{ID: 779, State: "DISMISSED", Body: PostBody(watch.Asker(), msgID, "r1"), SubmittedAt: "2026-11-12T20:55:00Z"})
	out, _ := run(t, h, tm, map[string]json.RawMessage{"r1": reqJSON(good())})
	if r := out.Results["r1"]; !r.OK || r.ReviewID != 779 || h.posts() != 0 {
		t.Fatalf("page 2 dismissed: %+v posts %d", r, h.posts())
	}
}

// The marker distinguishes requests: r1's marker is not r10's (the " -->" closes it). RED if the match loosens.
func TestTheMarkerIsExact(t *testing.T) {
	rs := []Review{{ID: 1, State: "APPROVED", Body: PostBody("A", "1234567890", "r10")}}
	if ours(rs, Marker("r1")) != nil || ours(rs, Marker("r10")) == nil {
		t.Fatal("marker match is not exact")
	}
	if LostAfter != 10*time.Minute {
		t.Fatalf("LostAfter %v", LostAfter)
	}
}

func noGrants() ([]grants.Grant, error) { return nil, nil }
