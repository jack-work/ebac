package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/source/author"
	"github.com/gluck/ebac/internal/source/github"
	"github.com/gluck/ebac/internal/source/teams"
)

const usage = `ebac — snapshot GitHub pull requests into figaro forms and wake a seat on the delta

  ebac init                                 install the systemd template units
  ebac add    --name N [--pr URL]...        create a critic (a role) over one or more PRs
              [--mode reviewer [--approve] [--clone DIR --worktree-root DIR] [--notes REF]]
  ebac add-teams --name N --chat ID --topic T --aria SEAT [--quiet "A,B"] [--every 60s]
                                            watch one Teams chat through tomb
  ebac add-author --name N --login L --repo O/R[,O/R] --since T [--until T] [--alert @ROLE] [--every 5m]
                                            watch one author's PRs: wake on a new PR or a merge
  ebac ls                                   every critic, its seat, and its health
  ebac show   --critic N                     one critic in detail
  ebac poll   --critic N [--dry-run]         run one reconciliation round (systemd calls this)
  ebac cast   --critic N --aria ID           seat or re-seat the role (succession)
  ebac stop   --critic N [--reason R]        stop polling; state is kept
  ebac resume --critic N
  ebac rm     --critic N [--purge]           archive and remove
  ebac ack    --critic N --round R           the seat says it has acted on a delta
  ebac post   --critic N --pr P [--reply-to T [--react +1] [--resolve] | --path F --line L] [--event E] --body-file F
                                            queue a comment or review on the form; the next round sends it
  ebac compact --critic N [--dry-run]        rewrite the form without comment bodies
  ebac migrate [--dry-run]                  move pre-form critics (critics/, snapshots/) into their forms
  ebac roster [--sync] [--cast ARIA]        the fleet view, as its own role
  ebac selfcheck [--critic N]                harness invariants, for develop mode
  ebac issue  --critic N --title T --body-file F   file a harness defect
  ebac queue add --pr URL [--priority N]    enqueue a PR (the queue is a FORM)
  ebac queue ls | rm ID | config --max-active N | promote [--dry-run]
  ebac reconcile [--dry-run]                rectify drift between critics/, forms and timers

Global flags: --gh <path> --socket <angelus.sock> --host <ghe host>
`

type globals struct {
	gh     string
	figaro string
	host   string
}

func init() {
	for _, s := range []poll.Source{github.Source{}, teams.Source{}, author.Source{}} {
		poll.Register(s)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	g := globals{
		gh:     env("EBAC_GH", "gh"),
		figaro: os.Getenv("EBAC_SOCKET"),
		host:   env("EBAC_HOST", forge.DefaultForgeHost()),
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	// Strip global flags wherever they appear.
	args = extractGlobals(args, &g)

	ctx := context.Background()
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "add-teams":
		err = cmdAddTeams(ctx, g, args)
	case "add-author":
		err = cmdAddAuthor(ctx, g, args)
	case "add":
		err = cmdAdd(ctx, g, args)
	case "set-write":
		err = cmdSetWrite(ctx, g, args)
	case "ls", "list":
		err = cmdLs(ctx, g, args)
	case "show":
		err = cmdShow(ctx, g, args)
	case "poll":
		err = cmdPoll(ctx, g, args)
	case "cast":
		err = cmdCast(ctx, g, args)
	case "stop":
		err = cmdStopResume(ctx, g, args, true)
	case "resume":
		err = cmdStopResume(ctx, g, args, false)
	case "rm", "remove":
		err = cmdRm(ctx, g, args)
	case "post":
		err = cmdPost(ctx, g, args)
	case "migrate":
		err = cmdMigrate(ctx, g, args)
	case "compact":
		err = cmdCompact(ctx, g, args)
	case "ack":
		err = cmdAck(ctx, g, args)
	case "roster":
		err = cmdRoster(ctx, g, args)
	case "selfcheck":
		err = cmdSelfcheck(ctx, g, args)
	case "issue":
		err = cmdIssue(ctx, g, args)
	case "queue":
		err = cmdQueue(ctx, g, args)
	case "reconcile":
		err = cmdReconcile(ctx, g, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "ebac: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func extractGlobals(args []string, g *globals) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--gh" && i+1 < len(args):
			g.gh, i = args[i+1], i+1
		case a == "--socket" && i+1 < len(args):
			g.figaro, i = args[i+1], i+1
		case a == "--host" && i+1 < len(args):
			g.host, i = args[i+1], i+1
		case strings.HasPrefix(a, "--gh="):
			g.gh = strings.TrimPrefix(a, "--gh=")
		case strings.HasPrefix(a, "--socket="):
			g.figaro = strings.TrimPrefix(a, "--socket=")
		case strings.HasPrefix(a, "--host="):
			g.host = strings.TrimPrefix(a, "--host=")
		default:
			out = append(out, a)
		}
	}
	return out
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// resolveBin turns a command name into an absolute path, now, in the
// operator's shell — never at poll time in a unit that has no PATH.
func resolveBin(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty binary name")
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("%s: %w", abs, err)
		}
		return abs, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%q is not on PATH: pass an absolute path with --gh/--figaro", name)
	}
	return filepath.Abs(p)
}

// newFlagSet keeps subcommand flag handling uniform.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}
