package poll

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

type spendCaller struct{ rep *plugin.Reply }

func (c spendCaller) Call(context.Context, string, plugin.Request) (*plugin.Reply, error) {
	// Through the wire, as a plugin process answers.
	b, _ := json.Marshal(c.rep)
	var r plugin.Reply
	_ = json.Unmarshal(b, &r)
	return &r, nil
}

// Every plugin call hands its reported spend to OnSpend (ebacd settles the budget with it); a reply with none
// calls nothing; a hand poll (OnSpend nil) is unaffected. RED if call() stops forwarding Spend.
func TestCallForwardsTheRoundsSpend(t *testing.T) {
	w := &model.Critic{Name: "author-jack", Source: "github-author"}
	sp := []model.Spend{{Limit: "graphql", N: 3, Remaining: 4100}, {Limit: "rest", N: 2, Remaining: -1}}
	var got [][]model.Spend
	p := &Poller{Plugins: spendCaller{&plugin.Reply{Spend: sp}}, OnSpend: func(s []model.Spend) { got = append(got, s) }}
	if _, err := p.call(context.Background(), w, plugin.Request{Op: plugin.OpRound}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != sp[0] || got[0][1] != sp[1] {
		t.Fatalf("forwarded %+v, want %+v", got, sp)
	}
	got = nil
	p.Plugins = spendCaller{&plugin.Reply{}}
	if _, err := p.call(context.Background(), w, plugin.Request{Op: plugin.OpRound}); err != nil || got != nil {
		t.Fatalf("a reply without spend forwarded %+v (%v)", got, err)
	}
	p.OnSpend, p.Plugins = nil, spendCaller{&plugin.Reply{Spend: sp}}
	if _, err := p.call(context.Background(), w, plugin.Request{Op: plugin.OpRound}); err != nil {
		t.Fatalf("hand poll: %v", err)
	}
}

// A form that claims write never authorizes a post once the overlay says read-only (positive control: write=true
// does). RED if Authorize trusts a form value (it reads the critic after Trusted/Authority overlay).
func TestAuthorizeReadsTheOverlaidWrite(t *testing.T) {
	post := &model.Post{PR: "a/b#1", Body: "x"}
	ro := &model.Critic{Name: "c", Mode: model.ModeReview, Write: false, PRs: []model.PRRef{{Owner: "a", Repo: "b", Number: 1}}}
	if why := Authorize(ro, &model.Snapshot{}, "me", post); !strings.Contains(why, "read-only") {
		t.Fatalf("read-only critic authorized: %q", why)
	}
}

// Poll of a critic with no trusted authority is refused before any plugin call (review f0f3a1d9 M2).
func TestPollRefusesNoAuthority(t *testing.T) {
	w := &model.Critic{Name: "x", FormID: "@f", NoAuthority: true}
	if err := refuseNoAuthority(w); err == nil || !strings.Contains(err.Error(), "no trusted authority for @f") {
		t.Fatalf("%v", err)
	}
	if err := refuseNoAuthority(&model.Critic{Name: "y"}); err != nil {
		t.Fatalf("control: %v", err)
	}
}
