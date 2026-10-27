// Package forgetest is the fake gh shared by tests outside internal/forge:
// a package's tests call FakeGH and declare
//
//	func TestHelperProcess(t *testing.T) { forgetest.HelperProcess(t) }
//
// because the fake re-execs that package's own test binary.
package forgetest

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/forge"
)

// FakeGH points GH.run at THIS test binary, re-executed as a child process
// (the same helper-process trick os/exec's own tests use), rather than an
// external shell script. A script with a `#!/usr/bin/env bash` shebang works
// on a developer machine but not inside the Nix build sandbox's checkPhase --
// there is no /usr/bin/env or /bin/bash there to resolve, so the shebang
// lookup fails and reports ENOENT confusingly on the SCRIPT's own path, not
// the missing interpreter. Re-execing the already-built Go test binary needs
// no interpreter at all.
func FakeGH(t *testing.T, scenario string) *forge.GH {
	t.Helper()
	orig := forge.ExecCommandContext
	forge.ExecCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", scenario}, args...)
		return exec.CommandContext(ctx, os.Args[0], cs...)
	}
	t.Cleanup(func() { forge.ExecCommandContext = orig })
	g := forge.NewGH("unused", "microsoft.ghe.com")
	g.WriteGap = 0
	g.NoCred = true // the fake gh needs no token
	return g
}

// HelperProcess, called from TestHelperProcess, is not a real test. It is the child process FakeGH
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
func HelperProcess(t *testing.T) {
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
