package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
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
	orig := ExecCommandContext
	ExecCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", scenario}, args...)
		return exec.CommandContext(ctx, os.Args[0], cs...)
	}
	t.Cleanup(func() { ExecCommandContext = orig })
	g := NewGH("unused", "microsoft.ghe.com")
	g.WriteGap = 0
	g.NoCred = true // the fake gh needs no token
	return g
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
	case "author":
		if p := os.Getenv("EBAC_FAKE_LOG"); p != "" {
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			fmt.Fprintf(f, "CALL %s\n", strings.Join(rest, " "))
			f.Close()
		}
		os.Stdout.WriteString(`[{"number":61586,"url":"https://x/pull/61586","title":"feat: one","state":"OPEN","isDraft":false,` +
			`"headRefOid":"ffff8f68aaaabbbbcccc","createdAt":"2026-10-22T20:43:20Z","updatedAt":"2026-10-22T21:00:00Z","mergedAt":"0001-01-01T00:00:00Z"},` +
			`{"number":61434,"url":"https://x/pull/61434","title":"feat: two","state":"MERGED","isDraft":false,` +
			`"headRefOid":"ce1bd297dddd","createdAt":"2026-10-22T19:29:02Z","updatedAt":"2026-10-22T20:56:29Z","mergedAt":"2026-10-22T20:56:29Z"}]`)
	case "pending":
		os.Stdout.WriteString(pendingJSON)
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
	case "post-422":
		os.Stderr.WriteString("gh: Validation Failed (HTTP 422)\n")
		os.Exit(1)
	case "post-ok", "resolve-fail", "reply-pending":
		joined := strings.Join(rest, " ")
		resolving := strings.Contains(joined, "resolveReviewThread")
		reacting := strings.Contains(joined, "addReaction")
		if scenario == "resolve-fail" && resolving {
			os.Stderr.WriteString("HTTP 502\n")
			os.Exit(1)
		}
		in, _ := io.ReadAll(os.Stdin)
		f, _ := os.OpenFile(os.Getenv("EBAC_FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "CALL %s\t%s\n", joined, in)
		f.Close()
		if resolving {
			os.Stdout.WriteString("true\n")
		} else if strings.Contains(joined, "addPullRequestReviewThreadReply") {
			state := "SUBMITTED"
			if scenario == "reply-pending" {
				state = "PENDING"
			}
			fmt.Fprintf(os.Stdout, `{"url":"https://x/pull/1#posted","state":%q,"pullRequestReview":{"databaseId":213120916}}`+"\n", state)
		} else if reacting {
			for _, a := range rest {
				if v, ok := strings.CutPrefix(a, "c="); ok {
					os.Stdout.WriteString(v + "\n")
				}
			}
		} else {
			os.Stdout.WriteString("https://x/pull/1#posted\n")
		}
	case "post-fail":
		os.Stderr.WriteString("HTTP 502\n")
		os.Exit(1)
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

func reviewIDs(st *model.PRState) []string {
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

func TestForgeHost(t *testing.T) {
	ghe := model.PRRef{URL: "https://microsoft.ghe.com/bic/aether/pull/42612"}
	pub := model.PRRef{URL: "https://github.com/acme/widget/pull/1"}
	slug := model.PRRef{Owner: "acme", Repo: "widget", Number: 2}
	for _, tc := range []struct {
		name string
		refs []model.PRRef
		want string
		bad  bool
	}{
		{name: "url names the host", refs: []model.PRRef{ghe}, want: "microsoft.ghe.com"},
		{name: "slug takes the fallback", refs: []model.PRRef{slug}, want: "fallback.example"},
		{name: "no refs takes the fallback", want: "fallback.example"},
		{name: "two forges is an error", refs: []model.PRRef{ghe, pub}, bad: true},
	} {
		got, err := model.ForgeHost(tc.refs, "fallback.example")
		if tc.bad != (err != nil) || got != tc.want && !tc.bad {
			t.Errorf("%s: got %q, %v", tc.name, got, err)
		}
	}
}

const pendingJSON = `{"data":{"repository":{"pullRequest":{
  "number":1,"url":"https://x/pull/1","title":"t","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"","baseRefName":"main","author":{"login":"a","__typename":"User"},
  "headRefOid":"sha1",
  "pending":{"nodes":[{"id":"PRR_p","databaseId":213120916,"createdAt":"2026-10-20T04:20:23Z","author":{"login":"jack"},"comments":{"totalCount":344}}]},
  "reviews":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
    {"id":"T1","isResolved":false,"isOutdated":false,"path":"a.go","line":3,"comments":{"pageInfo":{"hasNextPage":false},"nodes":[
      {"id":"C1","url":"u1","body":"finding","state":"SUBMITTED","createdAt":"1","updatedAt":"1","author":{"login":"amy","__typename":"User"}},
      {"id":"C2","url":"u2","body":"fixed <!-- ebac:post:rw:p1 -->","state":"PENDING","createdAt":"2","updatedAt":"2","author":{"login":"jack","__typename":"User"}}]}}]}
}}}}`

func TestClientErrorIsFinalExceptRateLimits(t *testing.T) {
	for msg, final := range map[string]bool{
		"gh: Validation Failed (HTTP 422)":       true,
		"gh: Not Found (HTTP 404)":               true,
		"gh: API rate limit exceeded (HTTP 429)": false,
		"gh: Bad Gateway (HTTP 502)":             false,
		"gh timed out after 90s":                 false,
	} {
		if got := ClientError(errors.New(msg)) != ""; got != final {
			t.Errorf("%q: final=%v, want %v", msg, got, final)
		}
	}
}

func TestIsBot(t *testing.T) {
	for _, tc := range []struct {
		a    gqlAuthor
		want bool
	}{
		{gqlAuthor{Login: "copilot-pull-request-reviewer", Typename: "Bot"}, true},
		{gqlAuthor{Login: "some-app[bot]", Typename: "User"}, true},
		{gqlAuthor{Login: "coderabbitai", Typename: "User"}, true},
		{gqlAuthor{Login: "alice", Typename: "User"}, false},
	} {
		if got := isBot(tc.a); got != tc.want {
			t.Errorf("isBot(%s) = %v, want %v", tc.a.Login, got, tc.want)
		}
	}
}
