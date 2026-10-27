package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gluck/ebac/internal/brief"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// ---------- add ----------

func cmdAdd(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	var prs stringList
	name := fs.String("name", "", "critic name (becomes the systemd instance)")
	fs.Var(&prs, "pr", "pull request URL or owner/repo#number (repeatable)")
	repo := fs.String("repo", "", "discover PRs from this repo, e.g. acme/widget")
	author := fs.String("author", "", "with --repo: restrict to this author (@me works)")
	mode := fs.String("mode", "review", "review | develop | reviewer")
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
	approve := fs.Bool("approve", false, "reviewer mode: may approve PRs it did not author")
	clone := fs.String("clone", "", "reviewer mode: local clone to fetch PR heads through")
	wtRoot := fs.String("worktree-root", "", "reviewer mode: check out each PR head under this directory")
	notes := fs.String("notes", "", "reviewer mode: repo-specific guidance the seat reads first (path or skill)")
	_ = fs.Parse(args)

	if err := model.ValidName(*name); err != nil {
		return err
	}
	if len(prs) == 0 && *repo == "" {
		return fmt.Errorf("give at least one --pr, or --repo to discover them")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists (ebac show --critic %s)", *name, *name)
	}
	m := model.Mode(*mode)
	if m != model.ModeReview && m != model.ModeDevelop && m != model.ModeReviewer {
		return fmt.Errorf("--mode must be review, develop or reviewer, got %q", *mode)
	}
	if m != model.ModeReviewer && (*approve || *clone != "" || *wtRoot != "" || *notes != "") {
		return fmt.Errorf("--approve, --clone, --worktree-root and --notes need --mode reviewer")
	}
	if (*clone == "") != (*wtRoot == "") {
		return fmt.Errorf("--clone and --worktree-root go together")
	}
	if m == model.ModeReviewer {
		if _, err := brief.ReadSignature(); err != nil {
			return err
		}
	}

	fig := st.Fig
	if _, err := resolveBin(g.gh); err != nil {
		return fmt.Errorf("gh: %w", err)
	}
	w := &model.Critic{
		Name: *name, Mode: m, CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		DiscoverRepo: *repo, DiscoverAuthor: *author, IssueRepo: *issueRepo,
		Write: *write, Approve: *approve, Notes: *notes,
		Stop: model.DefaultStop(), EverySec: int(interval.Seconds()),
	}
	if *clone != "" {
		w.Worktree = &model.Worktree{Clone: *clone, Root: *wtRoot}
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
		ref, err := model.ParsePRRef(s)
		if err != nil {
			return err
		}
		w.AddPR(ref)
	}
	if w.Host, err = model.ForgeHost(w.PRs, g.host); err != nil {
		return err
	}
	if err := forge.CheckForgeHost(w.Host); err != nil {
		return err
	}
	gh := forge.NewGH(g.gh, w.Host)
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

	if err := st.SaveCritic(ctx, w); err != nil {
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
		if err := model.ValidAriaID(seat); err != nil {
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

	fmt.Printf("critic %q created: role %s, %d PR(s), mode %s\n", w.Name, formID, len(w.PRs), w.Mode)
	for _, r := range w.PRs {
		fmt.Printf("  %s\n", r.Key())
	}
	fmt.Printf("stop when: %s\n", brief.DescribeStop(w.Stop))

	if *arm {
		if err := systemd.EnableTimer(w.Name, *interval); err != nil {
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

// cmdSetWrite toggles a critic's authority to act on its own pull requests.
//
// Per-critic and off by default. A fleet-wide default would mean every critic
// could act on every PR it watches, which is the failure the read-only brief
// was written to prevent; this makes the grant an explicit, named decision the
// operator takes one critic at a time.
func cmdSetWrite(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("set-write", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	on := fs.Bool("on", false, "grant reply/comment/push on this critic's PRs")
	off := fs.Bool("off", false, "revoke it")
	_ = fs.Parse(args)

	if *on == *off {
		return fmt.Errorf("give exactly one of --on or --off")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	w.Write = *on
	if err := st.SaveCritic(ctx, w); err != nil {
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

// ---------- cast / stop / resume / rm ----------

func cmdCast(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("cast", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	aria := fs.String("aria", "", "aria id to seat")
	mint := fs.Bool("mint", false, "mint a fresh aria instead")
	outfits := fs.String("outfit", "", "outfit names for a minted aria")
	_ = fs.Parse(args)
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	fig := st.Fig
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
	if err := model.ValidAriaID(seat); err != nil {
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
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	w.Runtime.Stopped = stop
	if stop {
		w.Runtime.StopReason = *reason
		_ = systemd.DisableTimer(w.Name)
	} else {
		w.Runtime.StopReason = ""
		w.Runtime.ConsecutiveFailures = 0
	}
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
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
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	_ = systemd.DisableTimer(w.Name)
	if err := systemd.RemoveTimerDropIns(w.Name); err != nil {
		fmt.Fprintf(os.Stderr, "ebac: %s: %v\n", w.Name, err)
	}
	snap, _ := st.LoadSnapshot(ctx, w.Name)
	w.Runtime.Stopped, w.Runtime.Archived = true, true
	w.Runtime.ArchivedAt = time.Now().UTC()
	if w.Runtime.StopReason == "" {
		w.Runtime.StopReason = "removed by operator"
	}
	dir, err := st.Archive(ctx, w, snap)
	if err != nil {
		return err
	}
	fmt.Printf("%s: archived to %s\n", w.Name, dir)
	if !*purge {
		if err := st.SaveCritic(ctx, w); err != nil {
			return err
		}
	}
	if *purge {
		fig := st.Fig
		if err := fig.FormRemove(ctx, w.FormID); err != nil {
			fmt.Fprintf(os.Stderr, "ebac: could not remove form %s: %v\n", w.FormID, err)
		} else {
			fmt.Printf("removed role form %s\n", w.FormID)
		}
	} else {
		fmt.Printf("role form %s left standing; it expires with its lease (or: figaro kill %s)\n", w.FormID, w.FormID)
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
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	fig := st.Fig
	// Flat scalar keys ONLY: an aria writing ack.round into a form whose
	// subtrees were written as literals would create a decoy key.
	if err := fig.SetJSON(ctx, w.FormID, "ack_round", *round); err != nil {
		return err
	}
	if err := fig.SetJSON(ctx, w.FormID, "ack_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if *note != "" {
		if err := fig.SetJSON(ctx, w.FormID, "ack_note", model.FirstLine(*note)); err != nil {
			return err
		}
	}
	fmt.Printf("%s: acked round %d\n", w.Name, *round)
	return nil
}
