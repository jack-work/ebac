package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
)

const usage = `ebac — snapshot GitHub pull requests into figaro forms and wake a seat on the delta

  ebac init                                 install the systemd template units
  ebac add    --name N [--pr URL]...        create a critic (a role) over one or more PRs
              [--mode reviewer [--approve] [--clone DIR --worktree-root DIR] [--notes REF]]
  ebac set --critic N --every D             poll interval of a critic (every_sec); ebacd takes it at the next round
  ebac set --critic N --end SPEC            its end, replacing it: until=<date> | all-closed[:grace] | expires=<D> (comma-joined) | with-role[=@role] (standing); no "none"
  ebac set --critic N --renew | --purge-at <date>   restart an expiry from now | when an archived critic's forms go
  ebac set --critic N --ping-every D        collect mode on a live critic: ping the seat at most every D (0 = off)
  ebac set --critic N [--threads S] [--inbox S] [--mirror K]   a keel critic's scope (see add-keel); "" clears a key
  ebac add-teams --name N --chat ID|48:notes --topic T --aria SEAT [--every 60s] [--ping-every 30m (collect mode)]
                                            watch one Teams chat through tomb
  ebac add-author --name N --login L --repo O/R[,O/R] --since T [--until T] [--alert @ROLE] [--recent 50] [--every 5m]
              [--paths GLOB[,GLOB]] [--title-match REGEX]
                                            watch one author's N most recent PRs: wake on a new PR or a merge;
                                            with a filter, only PRs whose title matches OR whose files match
                                            (form keys paths, title_match; bad value -> last good + filter_error)
  ebac add-keel --name N --repo O/R [--paths GLOB[,GLOB]] [--aria SEAT | --alert @ROLE] --waiting-on "A[,B]"
              [--self "A,B"] [--every 45s]  watch one keel repo's comment threads: wake when A answers (5m with --feed=false)
              [--feed=false] [--keel-url https://H --token-cmd "hush oauth get ebac"]
              [--threads all|changed] [--inbox all|heads|last:N] [--mirror KEY[.FIELD],...]
                                            scope: what the form gets (keys threads_scope, inbox_scope, mirror;
                                            bad value -> last good + scope_error); wakes always see the full fetch;
                                            changed and last:N trade completeness for size (the brief names the
                                            full view)
  ebac add-band-hub [--name band-state] [--every 45s] [--utility leob,ross] [--socket /run/band-hub/sock]
                                            mirror the band hub (queues, nodes, dupes; slim) into a studied
                                            form (plugin ebac-bandhub); no seat, never a wake, writes only on change
  ebac add-approve --name ahmed-approve --inputs @FORM --chat 19:... --chat-alias ahmed --ask-from 8:orgid:<guid>
              --grant ebac-approve.aether --principal sbx-cheroot-sec --ping @ROLE [--every 1m]
                                            the approve path for a sandboxed principal: requests.<id> on the inputs
                                            form -> APPROVE on bic/aether after grant, scope and Teams-ask checks;
                                            writes requests.<id>.result (plugin ebac-approve); no seat
  ebac add-org-approve --board @FORM --logins A[,B] [--name org-approve] [--ping @ROLE] [--every 15m]
              [--grant ebac-approve.org --principal org-approve --approver jack] [--known-breaks C[,C]]
                                            ORG-APPROVE: one lean search per round over the org's open bic/aether
                                            PRs; each stall candidate is asked as q.oa-<pr>-<head7> on the board and
                                            filed as a band-grants request; its /grants/ approval by --approver is
                                            the tap: APPROVE at that head (plugin ebac-approve); no seat
  ebac set --critic N --logins A[,B] | --known-breaks C[,C]   an org-approve critic's org and known breaks
  ebac grant-request --credential C --ttl D --reason R [--socket S]
                                            file a band-grants request as ebac's principal; prints its id
  ebac add-scout --name effort-github --authors A[,B] --repo O/R --board @FORM --inputs @FORM [--every 2m]
              [--labels L[,L]] [--title-match REGEX] [--paths GLOB[,GLOB]]
              [--owner AUTHOR[/TITLE_REGEX]=@ROLE ...]
              [--claim-by LOGIN [--claim-phrase P] --claim-label L] [--files-title REGEX]
                                            the GitHub scout: one entry pr:<n> / issue:<n> on its own role form per
                                            relevant PR and issue of A, B (label OR title OR paths), merge_state from
                                            the inputs form's rule and the board's marks (read-only); on merge or
                                            close a ledger record to the inputs form's outbox, the entry kept 7 d;
                                            a material issue change (not inside a plan-sync block, not a bot) pings the
                                            owner role of each open PR linking the issue
  ebac add-keel-branches --name aether-work --repo aether/aether [--reviewer @ROLE] [--base main] [--prefix work/]
              [--every 2m] [--keel-url https://H --token-cmd CMD]
                                            one child form per keel branch <prefix><owner>/<topic>
                                            (ebac-<name>-<owner>--<topic>: head, base, merge_base, diffstat, files,
                                            commits, threads, reviewers); code_ready/ready are the holder's,
                                            pr_url/pr_number keel CI's; wakes the holder on a new comment, pings
                                            the reviewer when code_ready turns true and on each new head after;
                                            registry branches -> forms on the critic form; at most 100 forms
  ebac add-roster [--name roles] [--every 1m]
                                            the role roster: roles, holders, lieutenants, fork counts per role (10 min),
                                            derived from the aria store into a studied form (plugin ebac-roster);
                                            no seat, never a wake, writes only on change
  ebac add-releases --name N --repo O/R [--alert @ROLE | --aria SEAT] [--every 15m]
                                            watch a github.com repo's releases: one wake per new tag, with its bump
  ebac repoint --critic N --alert @new | --all-from @old --to @new [--dry-run]
                                            rewrite the role addresses critics store (alert roles, scout owners and
                                            warn role, keel-branches reviewer); the seat stays ebac cast's
  ebac ls                                   every critic, its seat, and its health
  ebac show   --critic N                     one critic in detail
  ebac poll   --critic N [--dry-run]         run one round now, by hand (ebacd runs them on schedule)
  ebac daemon [--config F] [--pool N] [--round-timeout D]
                                            ebacd: schedule every critic under the shared budget (one per node)
  ebac cast   --critic N --aria ID           seat or re-seat the role (succession)
  ebac stop   --critic N [--reason R]        stop polling; state is kept
  ebac resume --critic N
  ebac rm     --critic N [--purge]           archive and remove
  ebac ack    --critic N --round R           the seat says it has acted on a delta
  ebac review --critic N --doc D --reviewers R[,R] --author-role A   (keel) stamped review key
  ebac verdict --critic N --role R --verdict pass|changes|comment --rev REV [--note T]   (keel) stamped verdict
  ebac post   --critic N --pr P [--reply-to T [--react +1] [--resolve] | --path F --line L] [--event E] --body-file - < F
                                            queue a comment or review on the form; the next round sends it
  ebac compact --critic N [--dry-run]        rewrite the form without comment bodies (author watch: run one
                                            round now, keeping only tracked PRs; sizes before and after)
  ebac migrate [--dry-run]                  move pre-form critics (critics/, snapshots/) into their forms
  ebac roster [--sync] [--cast ARIA]        the fleet view, as its own role
  ebac selfcheck [--critic N]                harness invariants, for develop mode
  ebac issue  --critic N --title T --body-file - < F   file a harness defect
  ebac queue add --pr URL [--priority N]    enqueue a PR (the queue is a FORM)
  ebac queue ls | rm ID | config --max-active N | promote [--dry-run]
  ebac reconcile [--dry-run]                rectify drift between critics/, forms and timers

Trigger rules (every critic): its role form's key trigger, an ordered JSON list, first match wins; the holder edits
it with figaro set and ebac reads it every round. add-* writes the source's default set.
  [{"contains": ["Agent-Authored on behalf of", "Sent by Figaro"], "action": "wake"},
   {"from": "operator", "action": "record"},
   {"default": true, "action": "wake"}]
  match: from | not_from (display name, sender id, bare guid, or "operator" = the Boss), contains (case-insensitive,
         full text), regex (Go), kind; all given fields must match, a list matches on any entry
  action: wake | record (inbox only) | drop (neither) | keep (the tier the source chose; PR, author and keel defaults)
  the last rule must be {"default": true, "action": ...}. A set that does not parse is never applied: the last good
  set stays in force and the form key trigger_error says why. ebac show prints the set in force.

Global flags: --gh <path> --socket <angelus.sock | unix:/path | wss://host/door> (default $EBAC_FIGARO, else the local socket; a wss door reads its token from the command in $EBAC_FIGARO_BEARER_CMD) --host <ghe host>
`

type globals struct {
	gh     string
	figaro string
	host   string
	// caller is the calling seat's aria, passed by the operator wrapper from
	// FIGARO_ARIA (sudo strips the environment). A claim: see internal/identity.
	caller string
}

func main() {
	caller, rest, cerr := splitCaller(os.Args[1:])
	if cerr != nil {
		fmt.Fprintln(os.Stderr, "ebac:", cerr)
		os.Exit(2)
	}
	if len(rest) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	g := globals{
		gh:     env("EBAC_GH", "gh"),
		figaro: os.Getenv("EBAC_SOCKET"),
		host:   env("EBAC_HOST", forge.DefaultForgeHost()),
		caller: caller,
	}
	cmd := rest[0]
	args := rest[1:]
	if err := mainGate(); err != nil {
		fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
		os.Exit(2)
	}

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
	case "add-keel":
		err = cmdAddKeel(ctx, g, args)
	case "verdict":
		err = cmdVerdict(ctx, g, args)
	case "review":
		err = cmdReview(ctx, g, args)
	case "add-releases":
		err = cmdAddReleases(ctx, g, args)
	case "repoint":
		err = cmdRepoint(ctx, g, args)
	case "add-scout":
		err = cmdAddScout(ctx, g, args)
	case "add-approve":
		err = cmdAddApprove(ctx, g, args)
	case "add-org-approve":
		err = cmdAddOrgApprove(ctx, g, args)
	case "grant-request":
		err = cmdGrantRequest(ctx, args)
	case "add-keel-branches":
		err = cmdAddKeelBranches(ctx, g, args)
	case "add-band-hub":
		err = cmdAddBandHub(ctx, g, args)
	case "add-roster":
		err = cmdAddRoster(ctx, g, args)
	case "add":
		err = cmdAdd(ctx, g, args)
	case "set":
		err = cmdSet(ctx, g, args)
	case "set-write":
		err = cmdSetWrite(ctx, g, args)
	case "ls", "list":
		err = cmdLs(ctx, g, args)
	case "show":
		err = cmdShow(ctx, g, args)
	case "poll":
		err = cmdPoll(ctx, g, args)
	case "daemon":
		err = cmdDaemon(ctx, g, args)
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
	if changesRoster(cmd, args) {
		syncRosterNow(ctx, g)
	}
}

// changesRoster: a command that adds, removes, stops or resumes a critic. Its
// roster row is written at once (the live board follows the roster form),
// not at the next 30-min reconcile. A help or dry run changes nothing.
func changesRoster(cmd string, args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" || strings.HasPrefix(a, "--dry-run") || strings.HasPrefix(a, "-dry-run") {
			return false
		}
	}
	switch cmd {
	case "add", "rm", "remove", "stop", "resume":
		return true
	}
	return strings.HasPrefix(cmd, "add-") && cmd != "add-roster"
}

// syncRosterNow rewrites the roster form; the command itself already
// succeeded, so a failure here is a warning, and reconcile repairs it later.
func syncRosterNow(ctx context.Context, g globals) {
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err == nil {
		_, _, err = fleet.SyncRoster(ctx, st, st.Fig, "")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ebac: roster not refreshed (reconcile will): %v\n", err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// splitCaller takes the caller from the FIRST argument only, in the one form
// the operator wrapper writes ("--caller=<aria>", before the user's args), so
// nothing the user types can override or swallow it. Any other --caller is
// refused.
func splitCaller(args []string) (caller string, rest []string, err error) {
	if len(args) > 0 && strings.HasPrefix(args[0], "--caller=") {
		caller, args = strings.TrimPrefix(args[0], "--caller="), args[1:]
		if caller == "" {
			return "", nil, fmt.Errorf("empty --caller")
		}
	}
	for _, a := range args {
		if a == "--caller" || strings.HasPrefix(a, "--caller=") {
			return "", nil, fmt.Errorf("--caller is set by the operator wrapper only (first argument); refused")
		}
	}
	return caller, args, nil
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
