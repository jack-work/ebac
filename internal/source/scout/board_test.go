package scout

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/sdk/model"
)

// setBoard writes the board's pr map and snapshot time as board-snapshot does: every open tracked PR of the world,
// its head, mergeability and checks.
func (w *world) setBoard(at time.Time, checks map[int]string) {
	prs := map[string]any{}
	for _, n := range []int{101, 102, 103, 201} {
		it := w.gh.items[n]
		ms := it.mergeState
		if ms == "" {
			ms = "BLOCKED"
		}
		c := map[string]any{}
		if s, ok := checks[n]; ok {
			c["Aether Runtime PR Pipeline"] = map[string]any{"state": s}
		}
		prs[itoa(n)] = map[string]any{"head": it.head, "state": "OPEN", "mergeable": it.mergeable, "merge_state": ms, "checks": c}
	}
	w.board = map[string]json.RawMessage{"pr": model.MustJSON(prs), "snapshot_at": model.MustJSON(at.UTC().Format(time.RFC3339))}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A settling PR the board carries is re-read only when its board state moves (Warden GO 11-17: PR state from the
// board, cost 0); off the board it takes the GitHub path.
func TestSettlingPRReReadOnlyOnABoardChange(t *testing.T) {
	w := newWorld(t)
	w.pass(0)
	w.pass(2 * time.Minute)

	w.setBoard(w.now, map[int]string{201: "pending"})
	w.st.PRs[201].Settling = true
	w.gh.queries = nil
	w.pass(2 * time.Minute) // first sight of the board: 201's board state is new to it
	if w.gh.calls != 2 || !strings.Contains(strings.Join(w.gh.queries, " "), "p201: pullRequest") {
		t.Fatalf("first board pass: %d calls, want the search + 201's re-read", w.gh.calls)
	}

	w.setBoard(w.now, map[int]string{201: "pending"})
	w.st.PRs[201].Settling = true
	r := w.pass(2 * time.Minute)
	if w.gh.calls != 1 {
		t.Fatalf("an unmoved board state re-read the PR: %d calls, log %v", w.gh.calls, r.Log)
	}

	w.setBoard(w.now, map[int]string{201: "success"})
	w.st.PRs[201].Settling = true
	w.gh.queries = nil
	w.pass(2 * time.Minute)
	if w.gh.calls != 2 || !strings.Contains(strings.Join(w.gh.queries, " "), "p201: pullRequest") {
		t.Fatalf("a moved board state: %d calls, want the search + 201's re-read", w.gh.calls)
	}

	// Control: a stale board stands for nothing; the settling PR takes the GitHub path (re-read, no ls-refs here).
	w.setBoard(w.now.Add(-time.Hour), map[int]string{201: "success"})
	w.st.PRs[201].Settling = true
	w.pass(2 * time.Minute)
	if w.gh.calls != 2 {
		t.Fatalf("stale board: %d calls, want the search + the GitHub re-read", w.gh.calls)
	}
}

// On a base move the board's mergeability stands for the PRs it carries: no mergeability read on GitHub; a PR the
// board shows turned CONFLICTING is re-read and its owner pinged. Control: a stale board, the mergeability read.
func TestBoardMergeabilityDrivesTheConflictPing(t *testing.T) {
	w := newWorld(t)
	w.watch.Owners = []model.OwnerRule{{Author: ours, Role: knight}, {Author: his, Role: secretary}}
	w.gh.heads = map[string]string{"main": "a1aaaaaaaa"}
	w.setBoard(w.now, nil)
	w.pass(0)
	w.setBoard(w.now, nil)
	w.pass(2 * time.Minute) // bases first seen: on the board, nothing asked
	if len(w.gh.light) != 0 {
		t.Fatalf("mergeability asked on GitHub for PRs the board carries: %q", w.gh.light)
	}

	w.gh.heads["main"] = "a2aaaaaaaa"
	w.gh.items[101].mergeable, w.gh.items[101].mergeState = "CONFLICTING", "DIRTY"
	w.setBoard(w.now, nil)
	w.gh.queries = nil
	r := w.pass(2 * time.Minute)
	if len(w.gh.light) != 0 || !strings.Contains(strings.Join(w.gh.queries, " "), "p101: pullRequest") {
		t.Fatalf("board conflict: light %q, queries %d, log %v", w.gh.light, len(w.gh.queries), r.Log)
	}
	if len(r.Pings) != 1 || r.Pings[0].To != knight || !strings.Contains(r.Pings[0].Text, "#101") {
		t.Fatalf("pings %v", r.Pings)
	}
	if r = w.pass(2 * time.Minute); len(r.Pings) != 0 || w.gh.calls != 1 {
		t.Fatalf("a standing board conflict: pings %v, %d calls", r.Pings, w.gh.calls)
	}

	// Control: a stale board; the base move asks GitHub for mergeability.
	w.gh.heads["main"] = "a3aaaaaaaa"
	w.setBoard(w.now.Add(-time.Hour), nil)
	w.pass(2 * time.Minute)
	if len(w.gh.light) == 0 {
		t.Fatal("stale board: no mergeability read on GitHub")
	}
}
