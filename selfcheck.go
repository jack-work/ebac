package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// The selfcheck is the instrument a develop-mode seat runs every heartbeat.
//
// Rules it obeys, taken from the prangl audit's own rulings because they were
// paid for once already:
//   - UNTESTABLE is a distinct result from WARN, and it carries the number it
//     WOULD have warned about. A check that cannot tell "not evaluable" from
//     "failing" trains an operator to ignore warnings.
//   - Every check prints what it MEASURED, not just its verdict.
//   - It prints at most ~20 lines. The source may be as long as it needs.
//   - A check that nobody can act on is deleted, never kept as decoration.

type CheckResult struct {
	Name    string
	Verdict string // OK | WARN | UNTESTABLE
	Detail  string
}

func Selfcheck(ctx context.Context, st *Store, fig *Figaro, out io.Writer, only string) (warns int, err error) {
	critics, err := st.ListCritics()
	if err != nil {
		return 0, err
	}
	if only != "" {
		var f []*Critic
		for _, w := range critics {
			if w.Name == only {
				f = append(f, w)
			}
		}
		critics = f
		if len(critics) == 0 {
			return 0, fmt.Errorf("no critic named %q", only)
		}
	}

	live := 0
	for _, w := range critics {
		if !w.Runtime.Archived {
			live++
		}
	}

	var results []CheckResult
	add := func(name, verdict, detail string) {
		results = append(results, CheckResult{name, verdict, detail})
	}

	// 1. Form exists for every live critic. A critic whose form is gone can
	//    still poll and will deliver to nobody, silently.
	missing := []string{}
	for _, w := range critics {
		if w.Runtime.Archived {
			continue
		}
		if _, e := fig.Form(ctx, w.FormID); e != nil {
			missing = append(missing, w.Name+"->"+w.FormID)
		}
	}
	if live == 0 {
		add("form-exists", "UNTESTABLE", "0 live critics to check")
	} else if len(missing) > 0 {
		add("form-exists", "WARN", fmt.Sprintf("%d/%d forms unreadable: %v", len(missing), live, missing))
	} else {
		add("form-exists", "OK", fmt.Sprintf("%d/%d forms readable", live, live))
	}

	// 2. Seat occupancy. A role with no target-aria collects deltas nobody
	//    will ever read.
	empty := []string{}
	dead := []string{}
	checked := 0
	for _, w := range critics {
		if w.Runtime.Archived {
			continue
		}
		form, e := fig.Form(ctx, w.FormID)
		if e != nil {
			continue
		}
		checked++
		t, _ := form["target-aria"].(string)
		if t == "" {
			empty = append(empty, w.Name)
		} else if !fig.AriaExists(ctx, t) {
			dead = append(dead, w.Name+"->"+t)
		}
	}
	switch {
	case checked == 0:
		add("seat-occupied", "UNTESTABLE", fmt.Sprintf("0 of %d live forms readable", live))
	case len(empty)+len(dead) > 0:
		add("seat-occupied", "WARN", fmt.Sprintf("%d/%d seats bad — empty:%v unreachable:%v", len(empty)+len(dead), checked, empty, dead))
	default:
		add("seat-occupied", "OK", fmt.Sprintf("%d/%d seats held", checked, checked))
	}

	// 3. Timer armed. The state directory and systemd can disagree, and a
	//    critic file with no timer never runs again.
	unarmed := []string{}
	for _, w := range critics {
		if w.Runtime.Archived || w.Runtime.Stopped {
			continue
		}
		if s := TimerState(w.Name); !s.Armed() {
			unarmed = append(unarmed, w.Name+"("+s.Display+")")
		}
	}
	if live == 0 {
		add("timer-armed", "UNTESTABLE", "0 live critics")
	} else if len(unarmed) > 0 {
		add("timer-armed", "WARN", fmt.Sprintf("%d/%d not armed: %v", len(unarmed), live, unarmed))
	} else {
		add("timer-armed", "OK", fmt.Sprintf("%d/%d armed", live, live))
	}

	// 4. Freshness. Stale beyond 4x the nominal interval means the timer is
	//    firing but the round is not completing.
	stale := []string{}
	for _, w := range critics {
		if w.Runtime.Archived || w.Runtime.Stopped {
			continue
		}
		if w.Runtime.LastSuccess.IsZero() {
			stale = append(stale, w.Name+"(never)")
			continue
		}
		if age := time.Since(w.Runtime.LastSuccess); age > 60*time.Minute {
			stale = append(stale, fmt.Sprintf("%s(%s)", w.Name, age.Round(time.Minute)))
		}
	}
	if live == 0 {
		add("freshness", "UNTESTABLE", "0 live critics")
	} else if len(stale) > 0 {
		add("freshness", "WARN", fmt.Sprintf("%d/%d stale >60m: %v", len(stale), live, stale))
	} else {
		add("freshness", "OK", fmt.Sprintf("%d/%d polled within 60m", live, live))
	}

	// 5. Stranded delivery debt: pending events with no retry scheduled and
	//    nobody able to receive them. Detect by PROPERTY (does this file owe
	//    a delivery nobody can invoke), never by enumerating causes.
	stranded := []string{}
	owing := 0
	for _, w := range critics {
		if len(w.Runtime.PendingEvents) == 0 {
			continue
		}
		owing++
		if w.Runtime.Archived || w.Runtime.Stopped || !TimerState(w.Name).Armed() {
			stranded = append(stranded, fmt.Sprintf("%s(%d events)", w.Name, len(w.Runtime.PendingEvents)))
		}
	}
	if owing == 0 {
		add("delivery-debt", "OK", "0 critics owe an undelivered batch")
	} else if len(stranded) > 0 {
		add("delivery-debt", "WARN", fmt.Sprintf("%d/%d debts unpayable: %v", len(stranded), owing, stranded))
	} else {
		add("delivery-debt", "OK", fmt.Sprintf("%d critic(es) owe a batch, all with a live timer", owing))
	}

	// 6. Flat-decoy detection. THE form hazard: writing a dotted path into a
	//    subtree that was written as a literal creates a flat sibling key
	//    that shadows nothing, and `unset` will then "succeed" on the decoy.
	//    Any key of ours containing a dot is that signature.
	decoys := []string{}
	owned := map[string]bool{"prs": true, "delta": true, "sync": true, "critic": true, "harness": true}
	formsRead := 0
	for _, w := range critics {
		if w.Runtime.Archived {
			continue
		}
		form, e := fig.Form(ctx, w.FormID)
		if e != nil {
			continue
		}
		formsRead++
		for k := range form {
			root, _, isDotted := strings.Cut(k, ".")
			if isDotted && owned[root] {
				decoys = append(decoys, w.FormID+":"+k)
			}
		}
	}
	if formsRead == 0 {
		add("no-flat-decoys", "UNTESTABLE", fmt.Sprintf("0 of %d forms readable", live))
	} else if len(decoys) > 0 {
		add("no-flat-decoys", "WARN", fmt.Sprintf("%d decoy key(s) across %d forms: %v — a dotted write hit a literal subtree", len(decoys), formsRead, decoys))
	} else {
		add("no-flat-decoys", "OK", fmt.Sprintf("%d forms clean", formsRead))
	}

	// 7. Archive owed: every PR closed, grace elapsed, still live.
	owed := []string{}
	evaluated := 0
	for _, w := range critics {
		if w.Runtime.Archived || !w.Stop.UntilAllClosed || w.Runtime.AllClosedSince == nil {
			continue
		}
		evaluated++
		grace := time.Duration(w.Stop.ArchiveGraceSec) * time.Second
		if time.Since(*w.Runtime.AllClosedSince) > grace+30*time.Minute {
			owed = append(owed, w.Name)
		}
	}
	if evaluated == 0 {
		add("archive-owed", "OK", "0 critics past their close grace")
	} else if len(owed) > 0 {
		add("archive-owed", "WARN", fmt.Sprintf("%d/%d overdue for archival: %v", len(owed), evaluated, owed))
	} else {
		add("archive-owed", "OK", fmt.Sprintf("%d in grace, none overdue", evaluated))
	}

	fmt.Fprintf(out, "ebac selfcheck — %d critic(es), %d live, %s\n",
		len(critics), live, time.Now().UTC().Format(time.RFC3339))
	for _, r := range results {
		fmt.Fprintf(out, "  %-10s %-16s %s\n", r.Verdict, r.Name, r.Detail)
		if r.Verdict == "WARN" {
			warns++
		}
	}
	fmt.Fprintf(out, "  %d WARN, %d UNTESTABLE of %d checks\n",
		warns, countVerdict(results, "UNTESTABLE"), len(results))
	return warns, nil
}

func countVerdict(rs []CheckResult, v string) int {
	n := 0
	for _, r := range rs {
		if r.Verdict == v {
			n++
		}
	}
	return n
}
