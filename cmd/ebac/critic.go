package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gluck/ebac/sdk/cfg/github"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/source/keel"
	"github.com/gluck/ebac/sdk/brief"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
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
	interval := fs.Duration("interval", 0, "heartbeat interval (default: 30m in reviewer mode, else 5m)")
	issueRepo := fs.String("issue-repo", "", "repo for develop-mode harness bug reports")
	maxRounds := fs.Int("max-rounds", 0, "stop after N rounds (0 = unbounded)")
	ttl := fs.Duration("ttl", 0, "stop after this much wall time (0 = unbounded)")
	grace := fs.Duration("archive-grace", 6*time.Hour, "keep polling this long after the last PR closes")
	noArchive := fs.Bool("no-archive", false, "do not archive when all PRs close")
	maxFail := fs.Int("max-failures", 20, "stop after N consecutive failed rounds (0 = never)")
	arm := fs.Bool("arm", true, "schedule it now (ebacd starts it within a minute); --arm=false creates it stopped, to stage only")
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
	if *interval <= 0 {
		*interval = model.DefaultEvery(m)
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

	if err := st.CreateCritic(ctx, w); err != nil {
		return err
	}
	if err := st.SeedTrigger(ctx, w); err != nil {
		return fmt.Errorf("critic created (role %s), but %w", w.FormID, err)
	}

	// Seat the role.
	seat := *aria
	if seat == "" && *mint {
		seat, err = fig.NewAria(ctx, *outfits)
		if err != nil {
			if figaro.IsRefused(err) {
				return fmt.Errorf("%s (critic created, unseated)", figaro.MintHint)
			}
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
		fmt.Printf("scheduled: ebacd polls it every %s, the first round within a minute\n", *interval)
	} else {
		fmt.Printf("\nstaged stopped (--arm=false). Dry-run first, then schedule it:\n")
		fmt.Printf("  ebac poll --critic %s --dry-run\n", w.Name)
		fmt.Printf("  ebac resume --critic %s\n", w.Name)
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
	if w.Write != *on {
		// #11: write is authority; it comes only from ebac's trusted config
		// (keyed by the role form id) or ebac's creation record, never from
		// a form or a later command (a passkey-grant raise is backlog #12).
		return fmt.Errorf("%s: write is set in ebac's trusted config (trusted.critics.%q.authority.write), not by ebac set-write", w.Name, w.FormID)
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

// cmdSet edits a live critic's configuration. --every D is its poll interval
// (every_sec), which ebacd takes at the critic's next round. --ping-every D turns COLLECT
// mode on (gather every round, ping the seat at most every D, only when
// something woke) or off (0). It must be 0 or at least the poll interval.
// --threads, --inbox and --mirror set a keel critic's scope (its form keys).
func cmdSet(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	pingEvery := fs.Duration("ping-every", -1, "COLLECT mode ping interval, e.g. 30m; 0 turns collect mode off")
	every := fs.Duration("every", 0, "poll interval, e.g. 5m: writes every_sec; ebacd takes it at the next round")
	threads, inbox, mirror := scopeFlags(fs)
	askName := fs.String("ask-name", "", "approve critic: the asker's name in the review body and ping (\"\" = the chat alias)")
	logins := fs.String("logins", "", "org-approve critic: the org's GHE logins, comma-separated (replaces the list)")
	breaks := fs.String("known-breaks", "", "org-approve critic: check names allowed red (main-wide known breaks), comma-separated; \"\" clears")
	end := fs.String("end", "", "the critic's end, replacing it: until=2026-12-01 | all-closed[:6h] | expires=30d (comma-joined: the earliest wins) | with-role[=@role] (standing, alone); no critic lives forever")
	renew := fs.Bool("renew", false, "restart an expires_after end from now")
	purgeAt := fs.String("purge-at", "", "an archived critic: when its forms are killed (2026-11-26 or RFC3339)")
	_ = fs.Parse(args)
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	scoped := given["threads"] || given["inbox"] || given["mirror"]
	ends := given["end"] || given["renew"] || given["purge-at"]
	if *pingEvery < 0 && !scoped && !ends && !given["ask-name"] && !given["every"] && !given["logins"] && !given["known-breaks"] {
		return fmt.Errorf("nothing to set: give --every D, --ping-every D (0 turns collect mode off), for a keel critic --threads, --inbox or --mirror, for an approve critic --ask-name, or for an org-approve critic --logins or --known-breaks")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	var set map[string]json.RawMessage
	var unset []string
	var scope model.KeelScope
	if scoped {
		if w.SourceName() != "keel-threads" {
			return fmt.Errorf("--threads, --inbox and --mirror are for keel critics; %s is %s", w.Name, w.SourceName())
		}
		vals := map[string]string{"threads": *threads, "inbox": *inbox, "mirror": *mirror}
		if set, unset, scope, err = overlayScope(w.Reads, given, vals); err != nil {
			return err
		}
	}
	if ends {
		now := time.Now().UTC()
		if given["end"] {
			if err := applyEnd(w, *end); err != nil {
				return err
			}
			if r := w.Stop.WithRole; r != "" {
				if err := fleet.IsRosterRole(ctx, st.Fig.Form, r); err != nil {
					return fmt.Errorf("--end with-role refused: %w", err)
				}
			}
		}
		if *renew {
			if w.Stop.ExpiresAfterSec <= 0 {
				return fmt.Errorf("%s has no expires_after end to renew (ebac set --end expires=30d)", w.Name)
			}
			w.Stop.RenewedAt = &now
		}
		if given["purge-at"] {
			if !w.Runtime.Archived {
				return fmt.Errorf("--purge-at is for an archived critic; %s is live (give it an end: --end)", w.Name)
			}
			t, err := parseWhen(*purgeAt)
			if err != nil {
				return fmt.Errorf("--purge-at %q: %w", *purgeAt, err)
			}
			w.Runtime.PurgeAt = &t
		}
		if err := st.SaveCritic(ctx, w); err != nil {
			return err
		}
		fmt.Printf("%s: end %s\n", w.Name, w.EndLabel())
	}
	if given["every"] {
		ping := time.Duration(w.PingEverySec) * time.Second
		if *pingEvery >= 0 {
			ping = *pingEvery // set in the same call: checked against the new interval below
		}
		if err := setEvery(w, *every, ping); err != nil {
			return err
		}
		if err := st.SaveCritic(ctx, w); err != nil {
			return err
		}
		if w.Runtime.Stopped {
			fmt.Printf("%s: polls every %s from its resume (stopped)\n", w.Name, w.Interval())
		} else {
			fmt.Printf("%s: polls every %s from its next round (ebacd)\n", w.Name, w.Interval())
		}
	}
	if given["ask-name"] {
		if w.Requests == nil {
			return fmt.Errorf("--ask-name is for approve critics; %s is %s", w.Name, w.SourceName())
		}
		w.Requests.AskName = strings.TrimSpace(*askName)
		if err := ghcfg.CheckApprove(w.Requests); err != nil {
			return err
		}
		if err := st.SaveCritic(ctx, w); err != nil {
			return err
		}
		fmt.Printf("%s: approvals now cite %q's ask; takes effect on the next round\n", w.Name, w.Requests.Asker())
	}
	if given["logins"] || given["known-breaks"] {
		if w.OrgApprove == nil {
			return fmt.Errorf("--logins and --known-breaks are for org-approve critics; %s is %s", w.Name, w.SourceName())
		}
		// #11: logins and known breaks are authority (whose PRs get approval
		// asks; which red checks count green): only ebac's trusted config.
		_, _ = logins, breaks
		return fmt.Errorf("%s: org-approve logins and known breaks are set in ebac's trusted config (trusted.critics.%q.authority), not by ebac set", w.Name, w.FormID)
	}
	if *pingEvery >= 0 {
		if err := checkPingEvery(*pingEvery, time.Duration(w.EverySec)*time.Second); err != nil {
			return err
		}
		w.PingEverySec = int(pingEvery.Seconds())
		if err := st.SaveCritic(ctx, w); err != nil {
			return err
		}
		fmt.Printf("%s: %s; takes effect on the next round\n", w.Name, describeCollect(w))
	}
	if scoped {
		if err := st.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
			return set, unset
		}); err != nil {
			return fmt.Errorf("%s: scope: %w", w.Name, err)
		}
		fmt.Printf("%s: scope %s; takes effect on the next round\n", w.Name, describeScope(scope))
	}
	return nil
}

// setEvery sets a critic's poll interval: at least MinEvery, a keel critic's
// own floor (a feed round holds its request), and never above a collect-mode
// ping interval (ping, 0 = off), which would then hold nothing back.
func setEvery(w *model.Critic, every, ping time.Duration) error {
	min := MinEvery
	if w.Keel != nil {
		min = max(min, keel.MinEvery(w.Keel.Feed))
	}
	if every < min {
		return fmt.Errorf("--every %s: want at least %s", every, min)
	}
	if ping > 0 && ping < every {
		return fmt.Errorf("--every %s: longer than the collect ping (%s); set --ping-every 0 or at least %s with it", every, ping, every)
	}
	w.EverySec = int(every.Seconds())
	return nil
}

// MinEvery is the shortest poll interval ebac set accepts.
const MinEvery = 10 * time.Second

// checkPingEvery: a ping interval shorter than the poll interval would never
// hold anything back.
func checkPingEvery(ping, every time.Duration) error {
	if ping < 0 || (ping != 0 && ping < every) {
		return fmt.Errorf("--ping-every %s: want 0 or at least the poll interval (%s)", ping, every)
	}
	return nil
}

// describeCollect is the collect-mode line of ebac show and ebac set.
func describeCollect(w *model.Critic) string {
	if w.PingEverySec <= 0 {
		return "collect off (wake on every round with news)"
	}
	s := fmt.Sprintf("collect mode, ping every %s", w.PingEvery())
	if !w.Runtime.LastPing.IsZero() {
		s += ", last ping " + w.Runtime.LastPing.UTC().Format(time.RFC3339)
	}
	return s
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
			if figaro.IsRefused(err) {
				return fmt.Errorf("%s", figaro.MintHint)
			}
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
	} else {
		w.Runtime.StopReason = ""
		w.Runtime.ConsecutiveFailures = 0
	}
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	fmt.Printf("%s: %sped (%s)\n", w.Name, verb, w.Runtime.StopReason)
	if !stop {
		fmt.Printf("ebacd schedules it within a minute\n")
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
		return err // a critic whose form is gone is not scheduled: nothing to remove
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
	// Saved even before a purge: ebac holds no kill grant, and a refused
	// purge must still leave the form renamed and seatless, to lapse.
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if *purge {
		// uid ebac holds no kill grant (GO-20261104-ebac-narrow): the lifetime broker removes ebac's own forms.
		if err := st.Fig.BrokerKill(ctx, w.FormID); err != nil {
			fmt.Fprintf(os.Stderr, "ebac: role form %s not removed: %v; it lapses with its lease (or: figaro kill %s)\n", w.FormID, err, w.FormID)
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
	// ONE write: read.<critic> = {round, at, note}. Three flat keys were three
	// form versions, three deltas to every studier, per ack (context audit
	// 2026-11-13). The whole read key is set as one value (never a dotted
	// path into it), so no decoy key can form.
	if err := fig.SetJSON(ctx, w.FormID, model.ReadKey, model.ReadMark(w.Name, *round, time.Now().UTC(), *note)); err != nil {
		return err
	}
	// The flat keys an older ack (or a seat following the old rule) left: once.
	if f, err := st.ReadForm(ctx, w.FormID); err == nil {
		for _, k := range model.LegacyAckKeys {
			if _, ok := f.Keys[k]; ok {
				if err := fig.Unset(ctx, w.FormID, k); err != nil {
					return err
				}
			}
		}
	}
	fmt.Printf("%s: acked round %d\n", w.Name, *round)
	return nil
}

// applyEnd replaces a critic's end with spec: comma-joined until=<date>,
// all-closed[:<grace>], expires=<D>. There is no "none": no critic lives
// forever (the Boss, 11-19); a spec that leaves no end is refused. Bad specs
// change nothing.
func applyEnd(w *model.Critic, spec string) error {
	st := w.Stop
	st.Deadline, st.ExpiresAfterSec, st.RenewedAt, st.UntilAllClosed, st.WithRole = nil, 0, nil, false, ""
	items := strings.Split(spec, ",")
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("--end: empty: give until=<date>, all-closed[:grace] or expires=<D>")
	}
	for _, it := range items {
		it = strings.TrimSpace(it)
		switch {
		case it == "with-role" || strings.HasPrefix(it, "with-role="):
			if len(items) != 1 {
				return fmt.Errorf("--end with-role stands alone: a standing critic has no calendar end")
			}
			role, given := strings.CutPrefix(it, "with-role=")
			if !given {
				role = w.AlertTarget()
			}
			if !strings.HasPrefix(role, "@") {
				return fmt.Errorf("--end with-role: %s has no alert role; give with-role=@<roster role>", w.Name)
			}
			st.WithRole = role
		case it == "none":
			return fmt.Errorf("--end none: refused: no critic lives forever; give it an end (expires=30d is renewable with --renew)")
		case strings.HasPrefix(it, "until="):
			t, err := parseWhen(strings.TrimPrefix(it, "until="))
			if err != nil {
				return fmt.Errorf("--end %s: %w", it, err)
			}
			st.Deadline = &t
		case it == "all-closed" || strings.HasPrefix(it, "all-closed:"):
			st.UntilAllClosed = true
			if g, ok := strings.CutPrefix(it, "all-closed:"); ok {
				d, err := parseDays(g)
				if err != nil || d < 0 {
					return fmt.Errorf("--end %s: grace: %v", it, err)
				}
				st.ArchiveGraceSec = int(d.Seconds())
			}
		case strings.HasPrefix(it, "expires="):
			d, err := parseDays(strings.TrimPrefix(it, "expires="))
			if err != nil || d < time.Hour {
				return fmt.Errorf("--end %s: want a duration of at least 1h, e.g. 30d", it)
			}
			st.ExpiresAfterSec = int(d.Seconds())
		default:
			return fmt.Errorf("--end %q: want until=<date>, all-closed[:grace], expires=<D> or with-role[=@role]", it)
		}
	}
	w.Stop = st
	return nil
}

// parseWhen reads a date (2026-12-01, midnight UTC) or an RFC3339 time.
func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
