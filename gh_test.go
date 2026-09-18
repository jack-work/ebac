package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeGH points GH.run at THIS test binary, re-executed as a child process
// (the same helper-process trick os/exec's own tests use), rather than an
// external shell script. A script with a `#!/usr/bin/env bash` shebang works
// on a developer machine but not inside the Nix build sandbox's checkPhase --
// there is no /usr/bin/env or /bin/bash there to resolve, so the shebang
// lookup fails and reports ENOENT confusingly on the SCRIPT's own path, not
// the missing interpreter. Re-execing the already-built Go test binary needs
// no interpreter at all.
func fakeGH(t *testing.T, scenario string) *GH {
	t.Helper()
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", scenario}, args...)
		return exec.CommandContext(ctx, os.Args[0], cs...)
	}
	t.Cleanup(func() { execCommandContext = orig })
	return NewGH("unused", "microsoft.ghe.com")
}

// TestHelperProcess is not a real test. It is the child process fakeGH
// re-execs; gated on argv rather than an env var because GH.run overwrites
// cmd.Env wholesale right after exec.CommandContext returns (`cmd.Env =
// append(os.Environ(), "GH_HOST="+g.Host, ...)`, in gh.go) -- a real, useful
// production behaviour (it is what lets every call fix GH_HOST regardless of
// what the caller's environment happened to hold), but it silently discarded
// GO_WANT_HELPER_PROCESS=1 set by an earlier version of this fake, so the
// child ran as an ordinary no-op test and `go test`'s own "PASS\n" summary
// landed on stdout where the fake JSON was expected -- a real external `gh`
// would never emit that, so decode failed on the 'P'. An ordinary `go test`
// run never has "--" in os.Args, so that alone is enough to gate on.
func TestHelperProcess(t *testing.T) {
	args := os.Args
	found := false
	for len(args) > 0 {
		a := args[0]
		args = args[1:]
		if a == "--" {
			found = true
			break
		}
	}
	if !found {
		return
	}
	defer os.Exit(0)
	if len(args) == 0 {
		os.Exit(2)
	}
	scenario, rest := args[0], args[1:]

	reviewsCursor := ""
	for _, a := range rest {
		if v, ok := strings.CutPrefix(a, "reviewsCursor="); ok {
			reviewsCursor = v
		}
	}

	switch scenario {
	case "paginate":
		if reviewsCursor == "" {
			os.Stdout.WriteString(page1JSON)
		} else {
			os.Stdout.WriteString(page2JSON)
		}
	case "logcursor":
		if strings.Contains(strings.Join(rest, " "), "reviewsCursor=") {
			os.Stdout.WriteString(emptyJSON)
		} else {
			os.Stderr.WriteString("reviewsCursor argument missing\n")
			os.Exit(1)
		}
	default:
		os.Exit(2)
	}
}

const page1JSON = `{"data":{"repository":{"pullRequest":{
  "number":1,"url":"https://x/pull/1","title":"t","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"","baseRefName":"main","author":{"login":"a","__typename":"User"},
  "headRefOid":"sha1",
  "reviews":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},
    "nodes":[{"id":"r1","state":"COMMENTED","submittedAt":"t1","body":"one","author":{"login":"x","__typename":"User"}},
             {"id":"r2","state":"COMMENTED","submittedAt":"t2","body":"two","author":{"login":"x","__typename":"User"}}]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}
}}}}`

const page2JSON = `{"data":{"repository":{"pullRequest":{
  "number":1,"url":"https://x/pull/1","title":"t","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"","baseRefName":"main","author":{"login":"a","__typename":"User"},
  "headRefOid":"sha1",
  "reviews":{"pageInfo":{"hasNextPage":false,"endCursor":""},
    "nodes":[{"id":"r3","state":"APPROVED","submittedAt":"t3","body":"three","author":{"login":"x","__typename":"User"}}]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}
}}}}`

const emptyJSON = `{"data":{"repository":{"pullRequest":{
  "number":1,"url":"https://x/pull/1","title":"t","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"","baseRefName":"main","author":{"login":"a","__typename":"User"},
  "headRefOid":"sha1",
  "reviews":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}
}}}}`

// Observed live on bic/aether#24485: by the time it had gone through a dozen
// automated review rounds it carried well over a hundred individual Review
// nodes, all fetched through a single `reviews(last:30)` with no cursor at
// all. FetchPR only ever looked at page 0, so anything before the newest 30
// was silently dropped -- with nothing in the response distinguishing "there
// are only 30" from "we didn't ask for the rest." A critic could report "no
// new reviews" while reviews genuinely existed just outside the window.
func TestFetchPRPaginatesReviewsNotJustTheNewest30(t *testing.T) {
	g := fakeGH(t, "paginate")
	st, err := g.FetchPR(context.Background(), "bic", "aether", 1)
	if err != nil {
		t.Fatalf("FetchPR: %v", err)
	}
	if !st.Complete {
		t.Fatalf("both pages exhausted their cursors; Complete should be true")
	}
	want := []string{"r1", "r2", "r3"}
	for _, id := range want {
		if _, ok := st.Reviews[id]; !ok {
			t.Errorf("review %s missing: only got %d review(s) (%v) -- reviews truncated to a single page again",
				id, len(st.Reviews), reviewIDs(st))
		}
	}
	if len(st.Reviews) != len(want) {
		t.Errorf("got %d reviews, want exactly %d: %v", len(st.Reviews), len(want), reviewIDs(st))
	}
}

func reviewIDs(st *PRState) []string {
	var out []string
	for id := range st.Reviews {
		out = append(out, id)
	}
	return out
}

// Guard against a copy-paste regression: the query must actually pass
// reviewsCursor as its own GraphQL variable, not silently reuse
// reviewThreads' cursor or drop it.
func TestFetchPRPassesReviewsCursorAsGHFlag(t *testing.T) {
	g := fakeGH(t, "logcursor")
	if _, err := g.FetchPR(context.Background(), "bic", "aether", 1); err != nil {
		t.Fatalf("FetchPR: %v (reviewsCursor argument likely missing from the query)", err)
	}
}
