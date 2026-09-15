package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `ebac — snapshot GitHub pull requests into figaro forms and wake a seat on the delta

  ebac init                                 install the systemd template units
  ebac add    --name N [--pr URL]...        create a critic (a role) over one or more PRs
  ebac ls                                   every critic, its seat, and its health
  ebac show   --critic N                     one critic in detail
  ebac poll   --critic N [--dry-run]         run one reconciliation round (systemd calls this)
  ebac cast   --critic N --aria ID           seat or re-seat the role (succession)
  ebac stop   --critic N [--reason R]        stop polling; state is kept
  ebac resume --critic N
  ebac rm     --critic N [--purge]           archive and remove
  ebac ack    --critic N --round R           the seat says it has acted on a delta
  ebac roster [--sync] [--cast ARIA]        the fleet view, as its own role
  ebac selfcheck [--critic N]                harness invariants, for develop mode
  ebac issue  --critic N --title T --body-file F   file a harness defect
  ebac queue add --pr URL [--priority N]    enqueue a PR (the queue is a FORM)
  ebac queue ls | rm ID | config --max-active N | promote [--dry-run]
  ebac reconcile [--prune] [--dry-run]      rectify drift between critics/, forms and timers
  ebac gc                                   reap forms whose critic is archived

Global flags: --gh <path> --socket <angelus.sock> --host <ghe host>
`

type globals struct {
	gh     string
	figaro string
	host   string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	g := globals{
		gh:     env("EBAC_GH", "gh"),
		figaro: os.Getenv("EBAC_SOCKET"),
		host:   env("EBAC_HOST", "github.com"),
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
	case "gc":
		err = cmdGC(ctx, g, args)
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

// ---------- init ----------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	bin := fs.String("bin", "", "path to the ebac binary the units should call")
	_ = fs.Parse(args)
	self := *bin
	if self == "" {
		if p, err := os.Executable(); err == nil {
			self = p
		}
	}
	dir, err := InstallUnits(self)
	if err != nil {
		return err
	}
	fmt.Printf("installed ebac@.service and ebac@.timer in %s\n", dir)
	fmt.Printf("units will exec: %s poll --critic <name>\n", self)
	return nil
}

// ---------- add ----------

func cmdAdd(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	var prs stringList
	name := fs.String("name", "", "critic name (becomes the systemd instance)")
	fs.Var(&prs, "pr", "pull request URL or owner/repo#number (repeatable)")
	repo := fs.String("repo", "", "discover PRs from this repo, e.g. acme/widget")
	author := fs.String("author", "", "with --repo: restrict to this author (@me works)")
	mode := fs.String("mode", "review", "review | develop")
	aria := fs.String("aria", "", "cast this existing aria into the role")
	mint := fs.Bool("mint", false, "mint a fresh aria and cast it")
	outfits := fs.String("outfit", "", "outfit names for a minted aria")
	interval := fs.Duration("interval", 5*time.Minute, "heartbeat interval")
	issueRepo := fs.String("issue-repo", "", "repo for develop-mode harness bug reports")
	maxRounds := fs.Int("max-rounds", 0, "stop after N rounds (0 = unbounded)")
	ttl := fs.Duration("ttl", 0, "stop after this much wall time (0 = unbounded)")
	grace := fs.Duration("archive-grace", 6*time.Hour, "keep polling this long after the last PR closes")
	noArchive := fs.Bool("no-archive", false, "do not archive when all PRs close")
	maxFail := fs.Int("max-failures", 20, "stop after N consecutive failed rounds (0 = never)")
	arm := fs.Bool("arm", false, "enable the systemd timer immediately")
	write := fs.Bool("write", false, "grant this critic authority to reply/comment/push on its PRs (never approve/merge/close)")
	_ = fs.Parse(args)

	if err := ValidName(*name); err != nil {
		return err
	}
	if len(prs) == 0 && *repo == "" {
		return fmt.Errorf("give at least one --pr, or --repo to discover them")
	}
	st, err := NewStore()
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(*name); err == nil {
		return fmt.Errorf("critic %q already exists (ebac show --critic %s)", *name, *name)
	}
	m := Mode(*mode)
	if m != ModeReview && m != ModeDevelop {
		return fmt.Errorf("--mode must be review or develop, got %q", *mode)
	}

	fig := NewFigaro(g.figaro)
	gh := NewGH(g.gh, g.host)

	ghAbs, err := resolveBin(g.gh)
	if err != nil {
		return fmt.Errorf("gh: %w", err)
	}
	w := &Critic{
		GHBin: ghAbs, FigaroSock: g.figaro,
		Name: *name, Mode: m, Host: g.host, CreatedAt: time.Now().UTC(),
		DiscoverRepo: *repo, DiscoverAuthor: *author, IssueRepo: *issueRepo,
		Write: *write,
		Stop:  DefaultStop(),
	}
	w.Stop.UntilAllClosed = !*noArchive
	w.Stop.ArchiveGraceSec = int(grace.Seconds())
	w.Stop.MaxRounds = *maxRounds
	w.Stop.MaxConsecutiveFailures = *maxFail
	if *ttl > 0 {
		d := time.Now().UTC().Add(*ttl)
		w.Stop.Deadline = &d
	}

	for _, s := range prs {
		ref, err := ParsePRRef(s)
		if err != nil {
			return err
		}
		w.AddPR(ref)
	}
	if *repo != "" {
		refs, err := gh.ListPRs(ctx, *repo, *author, "open", 100)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ebac: discovery failed at creation (the critic is still created and will retry): %v\n", err)
		} else {
			for _, r := range refs {
				w.AddPR(r)
			}
			fmt.Printf("discovered %d open PR(s) in %s\n", len(refs), *repo)
		}
	}

	formID, err := fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID

	if err := st.SaveCritic(w); err != nil {
		return err
	}

	// Seat the role.
	seat := *aria
	if seat == "" && *mint {
		seat, err = fig.NewAria(ctx, *outfits)
		if err != nil {
			return fmt.Errorf("mint aria: %w", err)
		}
		fmt.Printf("minted aria %s\n", seat)
	}
	if seat != "" {
		if err := ValidAriaID(seat); err != nil {
			return fmt.Errorf("critic created (role %s), but --aria is bad: %w", formID, err)
		}
		if err := fig.Cast(ctx, seat, formID); err != nil {
			// Report the partial rather than unwinding: the critic and the
			// form both exist and are named, so nothing is orphaned.
			fmt.Fprintf(os.Stderr, "ebac: critic created, but casting %s into %s failed: %v\n", seat, formID, err)
		} else {
			fmt.Printf("cast %s into %s\n", seat, formID)
		}
	}

	// Project once so the form is never empty, and so the seat's very first
	// context carries the contract.
	p := &Poller{Store: st, GH: gh, Fig: fig, Out: os.Stdout}
	p.projectAll(ctx, w, nil, &Delta{})

	fmt.Printf("critic %q created: role %s, %d PR(s), mode %s\n", w.Name, formID, len(w.PRs), w.Mode)
	for _, r := range w.PRs {
		fmt.Printf("  %s\n", r.Key())
	}
	fmt.Printf("stop when: %s\n", describeStop(w.Stop))

	if *arm {
		if err := EnableTimer(w.Name, *interval); err != nil {
			return fmt.Errorf("critic created but timer not armed: %w", err)
		}
		fmt.Printf("armed: ebac@%s.timer every %s\n", w.Name, *interval)
	} else {
		fmt.Printf("\nnot armed. Dry-run first, then arm:\n")
		fmt.Printf("  ebac poll --critic %s --dry-run\n", w.Name)
		fmt.Printf("  systemctl --user enable --now ebac@%s.timer\n", w.Name)
	}
	return nil
}

// ---------- ls / show ----------

func cmdLs(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("ls")
	asJSON := fs.Bool("j", false, "JSON")
	live := fs.Bool("live", false, "rebuild from source instead of reading the published index")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)

	var (
		rows   []RosterRow
		sum    RosterSummary
		age    time.Duration
		source = "index"
	)
	if *live {
		rows, sum, err = BuildRoster(ctx, st, fig, "")
		source = "live"
	} else {
		rows, sum, age, err = ReadRoster(ctx, st, fig)
		if err != nil {
			// Falling back is right, but doing it SILENTLY is not: an
			// operator must never mistake a rebuild for the index.
			fmt.Fprintf(os.Stderr, "ebac: %v; rebuilding live\n", err)
			rows, sum, err = BuildRoster(ctx, st, fig, "")
			source = "live (index unavailable)"
		}
	}
	if err != nil {
		return err
	}

	if *asJSON {
		dumpJSON(map[string]any{"source": source, "age_seconds": int(age.Seconds()), "summary": sum, "critics": rows})
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("no active critics (ebac add --name ...)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CRITIC\tMODE\tSEAT\tPRS\tOPEN\tUNRES\tROUND\tTIMER\tHEALTH")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			r.Critic, r.Mode, r.Seat, r.PRs, r.Open, r.Unresolved, r.Round, r.Timer, r.Health)
	}
	tw.Flush()
	fmt.Printf("\n%d critic(s), %d PRs (%d open), %d unresolved threads, %d awaiting a reply\n",
		sum.Critics, sum.PRs, sum.OpenPRs, sum.Unresolved, sum.Awaiting)

	switch {
	case source != "index":
		fmt.Printf("source: %s\n", source)
	case age > 45*time.Minute:
		// The reconciler runs every 30m. Past 45 the index is not merely
		// old, something has stopped.
		fmt.Printf("source: index, STALE by %s — is ebac-reconcile.timer running? (ebac ls --live)\n", age.Round(time.Minute))
	default:
		fmt.Printf("source: index, %s old\n", age.Round(time.Second))
	}
	if sum.Unhealthy > 0 {
		fmt.Printf("%d unhealthy — ebac selfcheck\n", sum.Unhealthy)
	}
	return nil
}

func cmdShow(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	snap, _ := st.LoadSnapshot(*name)
	if *asJSON {
		dumpJSON(map[string]any{"critic": w, "snapshot": snap})
		return nil
	}
	fig := NewFigaro(g.figaro)
	seat := "(empty)"
	if form, err := fig.Form(ctx, w.FormID); err == nil {
		if t, ok := form["target-aria"].(string); ok && t != "" {
			seat = t
		}
	}
	fmt.Printf("critic     %s (%s)\n", w.Name, w.Mode)
	fmt.Printf("role      %s -> %s\n", w.FormID, seat)
	fmt.Printf("timer     %s, next %s\n", TimerState(w.Name).Display, NextElapse(w.Name))
	fmt.Printf("round     %d, last success %s\n", w.Runtime.Round, stamp(w.Runtime.LastSuccess))
	fmt.Printf("stop when %s\n", describeStop(w.Stop))
	if w.Runtime.LastError != "" {
		fmt.Printf("error     %s\n", w.Runtime.LastError)
	}
	if n := len(w.Runtime.PendingEvents); n > 0 {
		fmt.Printf("pending   %d undelivered event(s), retry %d, next %s\n",
			n, w.Runtime.RetryCount, stamp(w.Runtime.NextRetryAt))
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PR\tSTATE\tHEAD\tTHREADS\tUNRES\tAWAITING\tTITLE")
	for _, r := range w.PRs {
		state, head, th, un, aw, title := "?", "-", 0, 0, 0, r.Title
		if snap != nil {
			if p, ok := snap.PRs[r.Key()]; ok {
				c := p.Counts("")
				state, head, th, un, aw, title = p.State, short(p.HeadSHA), c.Threads, c.Unresolved, c.AwaitingUs, p.Title
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", r.Key(), state, head, th, un, aw, firstLine(title))
	}
	tw.Flush()
	return nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------- poll ----------

func cmdPoll(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	dry := fs.Bool("dry-run", false, "reconcile and persist state, but never invoke figaro")
	all := fs.Bool("all", false, "poll every live critic in sequence")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	p := &Poller{Store: st, GH: NewGH(g.gh, g.host), Fig: NewFigaro(g.figaro), Out: os.Stdout, DryRun: *dry}
	if *all {
		ws, err := st.ListCritics()
		if err != nil {
			return err
		}
		var firstErr error
		for _, w := range ws {
			if w.Runtime.Archived {
				continue
			}
			if err := p.Poll(ctx, w.Name); err != nil {
				fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}
	if *name == "" {
		return fmt.Errorf("--critic is required (or --all)")
	}
	return p.Poll(ctx, *name)
}

// ---------- cast / stop / resume / rm ----------

func cmdCast(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("cast", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	aria := fs.String("aria", "", "aria id to seat")
	mint := fs.Bool("mint", false, "mint a fresh aria instead")
	outfits := fs.String("outfit", "", "outfit names for a minted aria")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)
	seat := *aria
	if seat == "" {
		if !*mint {
			return fmt.Errorf("give --aria <id> or --mint")
		}
		seat, err = fig.NewAria(ctx, *outfits)
		if err != nil {
			return err
		}
		fmt.Printf("minted aria %s\n", seat)
	}
	if err := ValidAriaID(seat); err != nil {
		return err
	}
	if !fig.AriaExists(ctx, seat) {
		return fmt.Errorf("aria %s does not resolve; nothing was cast", seat)
	}
	if err := fig.Cast(ctx, seat, w.FormID); err != nil {
		return err
	}
	fmt.Printf("%s: role %s now held by %s\n", w.Name, w.FormID, seat)
	fmt.Println("succession is immediate: target-aria is resolved late, per call, so the next heartbeat lands on the new seat.")
	return nil
}

func cmdStopResume(ctx context.Context, g globals, args []string, stop bool) error {
	verb := "stop"
	if !stop {
		verb = "resume"
	}
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	reason := fs.String("reason", "stopped by operator", "why")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	w.Runtime.Stopped = stop
	if stop {
		w.Runtime.StopReason = *reason
		_ = disableTimer(w.Name)
	} else {
		w.Runtime.StopReason = ""
		w.Runtime.ConsecutiveFailures = 0
	}
	if err := st.SaveCritic(w); err != nil {
		return err
	}
	p := &Poller{Store: st, GH: NewGH(g.gh, g.host), Fig: NewFigaro(g.figaro), Out: os.Stdout}
	snap, _ := st.LoadSnapshot(w.Name)
	_ = p.projectSync(ctx, w, snap, nil)
	fmt.Printf("%s: %sped (%s)\n", w.Name, verb, w.Runtime.StopReason)
	if !stop {
		fmt.Printf("re-arm with: systemctl --user enable --now ebac@%s.timer\n", w.Name)
	}
	return nil
}

func cmdRm(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	purge := fs.Bool("purge", false, "also remove the role form")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	_ = disableTimer(w.Name)
	snap, _ := st.LoadSnapshot(w.Name)
	w.Runtime.Stopped, w.Runtime.Archived = true, true
	w.Runtime.ArchivedAt = time.Now().UTC()
	if w.Runtime.StopReason == "" {
		w.Runtime.StopReason = "removed by operator"
	}
	dir, err := st.Archive(w, snap)
	if err != nil {
		return err
	}
	fmt.Printf("%s: archived to %s\n", w.Name, dir)
	if *purge {
		fig := NewFigaro(g.figaro)
		if err := fig.FormRemove(ctx, w.FormID); err != nil {
			fmt.Fprintf(os.Stderr, "ebac: could not remove form %s: %v\n", w.FormID, err)
		} else {
			fmt.Printf("removed role form %s\n", w.FormID)
		}
	} else {
		fmt.Printf("role form %s left standing (ebac gc, or figaro form rm %s)\n", w.FormID, w.FormID)
	}
	return nil
}

// ---------- ack ----------

func cmdAck(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("ack", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	round := fs.Int("round", 0, "the delta.round being acknowledged")
	note := fs.String("note", "", "one line: what you did or decided")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)
	// Flat scalar keys ONLY: an aria writing ack.round into a form whose
	// subtrees were written as literals would create a decoy key.
	if err := fig.SetJSON(ctx, w.FormID, "ack_round", *round); err != nil {
		return err
	}
	if err := fig.SetJSON(ctx, w.FormID, "ack_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if *note != "" {
		if err := fig.SetJSON(ctx, w.FormID, "ack_note", firstLine(*note)); err != nil {
			return err
		}
	}
	fmt.Printf("%s: acked round %d\n", w.Name, *round)
	return nil
}

// ---------- roster ----------

func cmdRoster(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("roster", flag.ExitOnError)
	sync := fs.Bool("sync", false, "rebuild the roster form from state")
	cast := fs.String("cast", "", "cast this aria into the roster role")
	mint := fs.Bool("mint", false, "mint an overseer aria and cast it")
	asJSON := fs.Bool("j", false, "JSON")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)
	if *sync || *cast != "" || *mint {
		id, sum, err := SyncRoster(ctx, st, fig, "")
		if err != nil {
			return err
		}
		fmt.Printf("roster %s: %d critic(es), %d PRs (%d open), %d unhealthy\n",
			id, sum.Critics, sum.PRs, sum.OpenPRs, sum.Unhealthy)
		seat := *cast
		if seat == "" && *mint {
			seat, err = fig.NewAria(ctx, "")
			if err != nil {
				return err
			}
			fmt.Printf("minted overseer %s\n", seat)
		}
		if seat != "" {
			if err := fig.Cast(ctx, seat, id); err != nil {
				return err
			}
			fmt.Printf("cast %s into the roster role %s\n", seat, id)
		}
		return nil
	}
	rows, sum, err := BuildRoster(ctx, st, fig, "")
	if err != nil {
		return err
	}
	if *asJSON {
		dumpJSON(map[string]any{"summary": sum, "critics": rows})
		return nil
	}
	dumpJSON(rows)
	return nil
}

// ---------- selfcheck / issue / gc ----------

func cmdSelfcheck(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ExitOnError)
	name := fs.String("critic", "", "restrict to one critic")
	strict := fs.Bool("strict", false, "exit nonzero if any check WARNs")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	warns, err := Selfcheck(ctx, st, NewFigaro(g.figaro), os.Stdout, *name)
	if err != nil {
		return err
	}
	if *strict && warns > 0 {
		return fmt.Errorf("%d check(s) warned", warns)
	}
	return nil
}

func cmdIssue(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	title := fs.String("title", "", "issue title")
	bodyFile := fs.String("body-file", "", "path to the issue body")
	repo := fs.String("repo", "", "override the critic's issue repo")
	var labels stringList
	fs.Var(&labels, "label", "label (repeatable)")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	target := *repo
	if target == "" {
		target = w.IssueRepo
	}
	if target == "" {
		return fmt.Errorf("no issue repo: pass --repo owner/name, or set one on the critic")
	}
	if *title == "" || *bodyFile == "" {
		return fmt.Errorf("--title and --body-file are both required")
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		return err
	}
	// Stamp provenance so a human reading the issue knows what produced it
	// and can reproduce the round.
	full := fmt.Sprintf("%s\n\n---\nFiled by ebac (critic `%s`, round %d, role %s) on %s.\n",
		strings.TrimRight(string(body), "\n"), w.Name, w.Runtime.Round, w.FormID,
		time.Now().UTC().Format(time.RFC3339))
	url, err := NewGH(g.gh, g.host).CreateIssue(ctx, target, *title, full, labels)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

func cmdGC(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "report only")
	_ = fs.Parse(args)
	st, err := NewStore()
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)
	entries, err := os.ReadDir(filepath.Join(st.Root, "archive"))
	if err != nil {
		return err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var w Critic
		if err := readJSON(filepath.Join(st.Root, "archive", e.Name(), "critic.json"), &w); err != nil {
			continue
		}
		if w.FormID == "" {
			continue
		}
		if _, err := fig.Form(ctx, w.FormID); err != nil {
			continue // already gone
		}
		n++
		if *dry {
			fmt.Printf("would remove %s (archived critic %s)\n", w.FormID, w.Name)
			continue
		}
		if err := fig.FormRemove(ctx, w.FormID); err != nil {
			fmt.Fprintf(os.Stderr, "ebac: %s: %v\n", w.FormID, err)
			continue
		}
		fmt.Printf("removed %s (archived critic %s)\n", w.FormID, w.Name)
	}
	if n == 0 {
		fmt.Println("nothing to collect")
	}
	// Leave the archive directories alone. This Order keeps its dead: the
	// evidence outlives the form.

	return nil
}

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


// cmdSetWrite toggles a critic's authority to act on its own pull requests.
//
// Per-critic and off by default. A fleet-wide default would mean every critic
// could act on every PR it watches, which is the failure the read-only brief
// was written to prevent; this makes the grant an explicit, named decision the
// operator takes one critic at a time.
func cmdSetWrite(_ context.Context, _ globals, args []string) error {
	fs := flag.NewFlagSet("set-write", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	on := fs.Bool("on", false, "grant reply/comment/push on this critic's PRs")
	off := fs.Bool("off", false, "revoke it")
	_ = fs.Parse(args)

	if *on == *off {
		return fmt.Errorf("give exactly one of --on or --off")
	}
	st, err := NewStore()
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(*name)
	if err != nil {
		return err
	}
	w.Write = *on
	if err := st.SaveCritic(w); err != nil {
		return err
	}
	if *on {
		fmt.Printf("%s: WRITE granted — the seat may reply, comment and push on:\n", w.Name)
		for _, p := range w.PRs {
			fmt.Printf("  %s\n", p.Key())
		}
		fmt.Println("approve / merge / close remain out of reach. Takes effect on the next round.")
	} else {
		fmt.Printf("%s: write revoked; the seat is read-only again from the next round.\n", w.Name)
	}
	return nil
}
