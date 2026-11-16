package keel

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

// scripted is four rounds of one repo: baseline; a boss answer, another
// author's reply, a resolve and a new two-comment thread; a quiet round; an
// edit. Feed mode gets the matching events.
func scripted() (rounds []map[string][]Thread, events [][]FeedEvent) {
	t1 := th("t1", false, cm("c1", "figaro-seat ⟩ jack", "question?"))
	t2 := th("t2", false, cm("o1", "Ana ⟩ ana", "a note"))
	t3 := th("t3", false, cm("b1", boss, "on b"))
	t3.Anchor = &Anchor{File: "other/b.md", StartLine: 3}
	at := func(ts ...Thread) map[string][]Thread {
		out := map[string][]Thread{}
		for _, t := range ts {
			t.Comments = append([]Comment(nil), t.Comments...)
			out[t.Anchor.File] = append(out[t.Anchor.File], t)
		}
		return out
	}
	rounds = append(rounds, at(t1, t2, t3))
	events = append(events, nil)

	t1.Comments = append(t1.Comments, cm("c2", boss, "yes, ship it"))
	t2.Comments = append(t2.Comments, cm("o2", "Ana ⟩ ana", "and another"))
	t3.Resolved = true
	t4 := th("t4", false, cm("h1", boss, "new thread head"), cm("h2", boss, "and a follow-up"))
	rounds = append(rounds, at(t1, t2, t3, t4))
	events = append(events, []FeedEvent{
		{Seq: 2, Kind: "comment.created", Path: "doc/a.md", Thread: "t1", Comment: "c2", Author: "jack", Signed: boss},
		{Seq: 3, Kind: "comment.created", Path: "doc/a.md", Thread: "t2", Comment: "o2", Author: "ana", Signed: "Ana ⟩ ana"},
		{Seq: 4, Kind: "thread.resolved", Path: "other/b.md", Thread: "t3"},
		{Seq: 5, Kind: "comment.created", Path: "doc/a.md", Thread: "t4", Comment: "h1", Author: "jack", Signed: boss},
		{Seq: 6, Kind: "comment.created", Path: "doc/a.md", Thread: "t4", Comment: "h2", Author: "jack", Signed: boss},
	})

	rounds = append(rounds, at(t1, t2, t3, t4))
	events = append(events, nil)

	t1.Comments[1].Body = "yes, ship it (edited)"
	rounds = append(rounds, at(t1, t2, t3, t4))
	events = append(events, []FeedEvent{{Seq: 7, Kind: "comment.edited", Path: "doc/a.md", Thread: "t1", Comment: "c2", Author: "jack", Signed: boss}})
	return rounds, events
}

// runScripted plays the script through the plugin with the given role-form
// keys and returns each round's reply, normalised (clock fields zeroed).
func runScripted(t *testing.T, feed bool, keys map[string]json.RawMessage) []*plugin.Reply {
	t.Helper()
	old := feedWait
	feedWait = 0
	t.Cleanup(func() { feedWait = old })
	rounds, events := scripted()
	f := &fakeKeel{events: []FeedEvent{{Seq: 1, Kind: "comment.created", Path: "doc/a.md", Thread: "t1", Comment: "c1", Author: "jack", Signed: "figaro-seat ⟩ jack"}}}
	cl := unixKeel(t, f)
	r := ref()
	r.Feed = feed
	w := model.Critic{Name: "golden", Mode: model.ModeKeel, Source: "keel-threads", FormID: "@golden", Keel: r}
	var out []*plugin.Reply
	for i := range rounds {
		f.mu.Lock()
		f.threads = rounds[i]
		f.events = append(f.events, events[i]...)
		f.mu.Unlock()
		w.Runtime.Round = i + 1
		rep, err := Plugin{Client: cl}.Handle(context.Background(), plugin.Request{Op: plugin.OpRound, Critic: w, Seat: "seat0001", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		w = *rep.Critic
		norm := *rep.Critic
		norm.Runtime.LastSuccess = time.Time{}
		rep.Critic = &norm
		for k, it := range rep.Inbox {
			it.At = "AT"
			rep.Inbox[k] = it
		}
		out = append(out, rep)
	}
	return out
}

// The control: with no scope keys a round is byte-identical to the round
// before scopes existed (golden captured at bf02820).
func TestDefaultRoundIsByteIdenticalToBeforeScopes(t *testing.T) {
	for _, mode := range []struct {
		name string
		feed bool
	}{{"threads", false}, {"feed", true}} {
		t.Run(mode.name, func(t *testing.T) {
			got, err := json.MarshalIndent(runScripted(t, mode.feed, nil), "", " ")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join("testdata", "golden", "default-"+mode.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("default %s rounds differ from %s:\n%s", mode.name, file, got)
			}
		})
	}
}
