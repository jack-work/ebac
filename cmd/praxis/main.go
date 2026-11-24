// Command praxis is the thin client for praxis issues: it writes figaro
// forms and nothing else. The gh-cache sync (ebac's github-cache critic)
// renders and creates the GitHub issue from those forms; praxis never
// calls GitHub.
//
//	praxis issue new    --topic T --k K --n N [--parent P] --summary S --delivers D [--contract-file F] [--assignee A]
//	praxis issue split  <parent> --child "summary|delivers" [--child ...]
//	praxis issue status <n> <claimed|split|in-progress|pr-open|done> [--pr N ...]
//	praxis issue done   <n> [--pr N ...]
//	praxis issue adopt  <n> --spec FILE
//
// Contract with the sync: the new-issue contract (kept with the operator notes).
// A new issue is one request key on the gh-cache register,
// ours.praxis_new.<id>; the sync answers in sync.praxis_new.<id> and mints
// the child form ebac-gh-cache-<repo>-issue-<n>, whose praxis.* group
// praxis owns.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	app := &App{
		Forms:    &FigaroForms{Bin: envOr("PRAXIS_FIGARO", "figaro")},
		Register: envOr("PRAXIS_REGISTER", "@ebac-gh-cache"),
		Repo:     os.Getenv("PRAXIS_REPO"),
		By:       os.Getenv("PRAXIS_BY"),
		Wait:     60 * time.Second,
		Poll:     2 * time.Second,
		Now:      time.Now,
		NewID:    randomID,
		Sleep:    time.Sleep,
		Out:      os.Stdout,
	}
	if app.Repo == "" {
		fmt.Fprintln(os.Stderr, "praxis: PRAXIS_REPO is required (owner/name)")
		os.Exit(2)
	}
	os.Exit(run(ctx, app, os.Args[1:], os.Stderr))
}

func run(ctx context.Context, app *App, args []string, errw io.Writer) int {
	if err := app.Dispatch(ctx, args); err != nil {
		fmt.Fprintln(errw, "praxis:", err)
		return 1
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
