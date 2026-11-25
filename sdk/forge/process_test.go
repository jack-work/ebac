package forge

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// #76: every GH of a process books into one process total, so a one-shot verb can print what it cost the shared
// pool. Two clients, a 3-point read and a mutation: the total is both. RED if a GH stops booking the process total.
func TestEveryClientsSpendIsBookedOnTheProcessTotal(t *testing.T) {
	ResetProcessSpent()
	t.Cleanup(ResetProcessSpent)
	ctx := context.Background()
	if _, err := fakeGH(t, "cost3").GraphQL(ctx, "query{ viewer{login} }"); err != nil {
		t.Fatal(err)
	}
	if _, err := fakeGH(t, "mutate").Mutate(ctx, "mutation{ x }", nil); err != nil {
		t.Fatal(err)
	}
	want := Spend{All: 2, Calls: 2, Points: 4, Remaining: 4700}
	if got := ProcessSpent(); got != want {
		t.Fatalf("process spend = %+v, want %+v", got, want)
	}
}

// A process that asked nothing has spent nothing, with no remaining known.
func TestAProcessThatAskedNothingSpentNothing(t *testing.T) {
	ResetProcessSpent()
	if got, want := ProcessSpent(), (Spend{Remaining: -1}); got != want {
		t.Fatalf("process spend = %+v, want %+v", got, want)
	}
}

type recPacer struct {
	events []string
	err    error
}

func (p *recPacer) Wait(context.Context) error {
	p.events = append(p.events, "wait")
	return p.err
}

func (p *recPacer) Settle(extra int) {
	p.events = append(p.events, "settle "+string(rune('0'+extra)))
}

// The pacer hears every GraphQL call before it runs: a read waits, then settles what it cost beyond the one point
// Wait reserved (3 -> 2); a mutation (one point) only waits. RED if a call path skips the pacer or settles wrong.
func TestThePacerHearsEveryGraphQLCall(t *testing.T) {
	ResetProcessSpent()
	t.Cleanup(ResetProcessSpent)
	p := &recPacer{}
	t.Cleanup(SetPacer(p))
	ctx := context.Background()
	if _, err := fakeGH(t, "cost3").GraphQL(ctx, "query{ viewer{login} }"); err != nil {
		t.Fatal(err)
	}
	if _, err := fakeGH(t, "mutate").Mutate(ctx, "mutation{ x }", nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"wait", "settle 2", "wait"}; !reflect.DeepEqual(p.events, want) {
		t.Fatalf("pacer events = %q, want %q", p.events, want)
	}
}

// A wait that ends (the caller's context) stops the call before gh runs: no call is booked. RED if a call runs
// after its pacer refused it.
func TestACallWhosePacerWaitEndsIsNotMade(t *testing.T) {
	ResetProcessSpent()
	t.Cleanup(ResetProcessSpent)
	p := &recPacer{err: context.Canceled}
	t.Cleanup(SetPacer(p))
	ctx := context.Background()
	if _, err := fakeGH(t, "cost3").GraphQL(ctx, "query{ viewer{login} }"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read err = %v, want context.Canceled", err)
	}
	if _, err := fakeGH(t, "mutate").Mutate(ctx, "mutation{ x }", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("mutate err = %v, want context.Canceled", err)
	}
	if got, want := ProcessSpent(), (Spend{Remaining: -1}); got != want {
		t.Fatalf("process spend = %+v, want %+v (no call made)", got, want)
	}
}

// A read is charged once dispatched (review r1 #4): a gh failure or an answer that is not JSON still books one call
// of one point, not a REST call. RED if the charge waits for a decoded answer.
func TestAReadThatFailsOrIsUnreadableIsStillChargedAsGraphQL(t *testing.T) {
	for _, scenario := range []string{"post-fail", "nonjson"} {
		ResetProcessSpent()
		if _, err := fakeGH(t, scenario).GraphQL(context.Background(), "query{ viewer{login} }"); err == nil {
			t.Fatalf("%s: no error", scenario)
		}
		if got, want := ProcessSpent(), (Spend{All: 1, Calls: 1, Points: 1, Remaining: -1}); got != want {
			t.Errorf("%s: process spend = %+v, want %+v", scenario, got, want)
		}
	}
	ResetProcessSpent()
}

// The outbox's GraphQL writes (a thread reply, a reaction, a resolve) take the paced, charged path (review r1 #3):
// three waits, three GraphQL calls of one point. RED if one of them calls gh around the hooks (booked as REST).
func TestOutboxGraphQLWritesArePacedAndChargedAsGraphQL(t *testing.T) {
	ResetProcessSpent()
	t.Cleanup(ResetProcessSpent)
	t.Setenv("EBAC_FAKE_LOG", filepath.Join(t.TempDir(), "calls.log"))
	p := &recPacer{}
	t.Cleanup(SetPacer(p))
	gh := fakeGH(t, "post-ok")
	ctx := context.Background()
	pr := &model.PRState{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc"}
	if url, err := gh.Post(ctx, pr, &model.Post{ReplyTo: "PRRT_1"}, "body"); err != nil || url != "https://x/pull/1#posted" {
		t.Fatalf("reply: %q %v", url, err)
	}
	if err := gh.React(ctx, "PRRC_1", "THUMBS_UP"); err != nil {
		t.Fatalf("react: %v", err)
	}
	if err := gh.ResolveThread(ctx, "PRRT_1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := []string{"wait", "wait", "wait"}; !reflect.DeepEqual(p.events, want) {
		t.Fatalf("pacer events = %q, want %q", p.events, want)
	}
	if got, want := ProcessSpent(), (Spend{All: 3, Calls: 3, Points: 3, Remaining: -1}); got != want {
		t.Fatalf("process spend = %+v, want %+v", got, want)
	}
}
