package main

import (
	"fmt"
	"strings"
)

// The brief is what actually costs money: it is a turn. Keep it short, state
// the delta, state the boundary, and get out of the way.
func buildBrief(w *Critic, snap *Snapshot, events []Event, me string, retry int) string {
	d := &Delta{Events: events}
	var b strings.Builder

	fmt.Fprintf(&b, "ebac %s round %d: %s\n", w.Name, w.Runtime.Round, d.Summary())
	if retry > 0 {
		fmt.Fprintf(&b, "(redelivery attempt %d; earlier sends failed, so this batch may repeat news you have)\n", retry+1)
	}
	b.WriteString("\n")

	t1 := d.Tier1()
	if len(t1) > 0 {
		b.WriteString("WORTH YOUR ATTENTION\n")
		for _, e := range t1 {
			fmt.Fprintf(&b, "  - %s\n", e.Line())
			if e.URL != "" {
				fmt.Fprintf(&b, "    %s\n", e.URL)
			}
		}
		b.WriteString("\n")
	}
	if bg := tier2(events); len(bg) > 0 {
		fmt.Fprintf(&b, "BACKGROUND (%d, no action expected)\n", len(bg))
		for i, e := range bg {
			if i >= 6 {
				fmt.Fprintf(&b, "  - ... and %d more\n", len(bg)-6)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", e.Line())
		}
		b.WriteString("\n")
	}

	if snap != nil && len(snap.PRs) > 0 {
		b.WriteString("STANDING\n")
		for _, k := range snap.SortedPRKeys() {
			p := snap.PRs[k]
			c := p.Counts(me)
			flag := ""
			if !p.Complete {
				flag = "  [SCAN INCOMPLETE — deletions not inferred this round]"
			}
			fmt.Fprintf(&b, "  %s %s  head %s  %d unresolved, %d awaiting a reply%s\n",
				k, p.State, short(p.HeadSHA), c.Unresolved, c.AwaitingUs, flag)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "If any value in your form ends in … or reads as unbalanced JSON, it was elided at %d chars.\n"+
		"Read it in full with `figaro form %s -j` before acting on it.\n\n", formValueElisionLimit, w.FormID)

	b.WriteString("BOUNDARY\n")
	if w.Write {
		b.WriteString("  You have WRITE on these pull requests: reply to threads, comment, and push\n")
		b.WriteString("  to the head branch. The operator granted this per-critic. You may NOT approve,\n")
		b.WriteString("  merge or close — those are irreversible and are not yours. Act only on the PRs\n")
		b.WriteString("  named above; any other PR is still read-only.\n\n")
	} else {
		b.WriteString("  You are READ-ONLY on these pull requests. Do not comment, review, approve,\n")
		b.WriteString("  request changes, push, label, close or merge. The operator did not ask you to\n")
		b.WriteString("  touch their PRs and you have no gate. Report; do not act.\n\n")
	}

	switch w.Mode {
	case ModeDevelop:
		b.WriteString(developCharge(w))
	default:
		b.WriteString(reviewCharge(w))
	}

	fmt.Fprintf(&b, "\nWhen finished: ebac ack --critic %s --round %d [--note '<one line>']\n",
		w.Name, w.Runtime.Round)
	b.WriteString("Acking is what stops this delta being re-raised. Ack even when you decided to do nothing.\n")
	return b.String()
}

func reviewCharge(w *Critic) string {
	var b strings.Builder
	b.WriteString("YOUR JOB\n")
	b.WriteString("  Read the delta. Decide whether anything here needs the operator. If it does, say so\n")
	b.WriteString("  in one or two sentences with the URL. If it does not, ack and stay silent.\n")
	b.WriteString("  A quiet round is a correct round.\n")
	if w.Write {
		b.WriteString(writeDiscipline())
	}
	return b.String()
}

// writeDiscipline is appended only for a critic that may act. A seat with write
// and a vague charge produces churn: it answers every finding with a commit,
// which draws another review, which draws another commit. These rules exist to
// make the seat converge instead.
func writeDiscipline() string {
	return "\n" +
		"HOW TO ACT (write-enabled)\n" +
		"  CONVERGE, DO NOT CHURN. Prefer settling a disagreement in the PR thread over\n" +
		"  answering it with a commit. Reviewers respond to what you give them: a long\n" +
		"  body invites a long review. Your output sets theirs.\n" +
		"  - VALIDATE every finding at the head yourself before accepting it. Execute the\n" +
		"    claim; do not take a premise on trust. Cite the SHA you read.\n" +
		"  - REJECT WITH A REASON anything wrong, tangential or vague. Vague improvements\n" +
		"    beget more feedback. Say what is out of scope and offer to file it.\n" +
		"  - CITE precisely: link the individual comment or thread id you are answering.\n" +
		"    Track which threads are open and which you have already answered.\n" +
		"  - ASK REVIEWERS FOR BREVITY. Name a line budget. Say why: the change is small\n" +
		"    and long analysis costs more than it returns.\n" +
		"  - NO INLINE RATIONALE IN CODE. No explanatory comments, no threat models, no\n" +
		"    review history in the source. Code only. Rationale belongs in the PR body or\n" +
		"    a comment. Do not match a surrounding file's comment density.\n" +
		"  - NEVER claim a test or build you did not run. Name what is unverified.\n" +
		"  - Approve, merge and close are never yours.\n" +
		"  When reviewers cohere on an approach, implement it. When they split, bring it\n" +
		"  to the operator rather than picking a winner.\n" +
		"\n" +
		"SET THE PROTOCOL EARLY, THEN HOLD IT\n" +
		"  Reviewers calibrate to what you give them. A PR that opens with a wall of\n" +
		"  rationale gets walls back. State the terms once, near the start, in the PR:\n" +
		"  - A LINE BUDGET, with the reason. \"Roughly 15 lines. This is a 400 line diff;\n" +
		"    long analysis costs more than it returns.\" Name the numbers you have.\n" +
		"  - WHAT IS OUT OF SCOPE, listed, so nobody spends a round on it. Offer to file\n" +
		"    each as its own issue.\n" +
		"  - THE OPEN QUESTIONS, numbered, and nothing else. Two findings that constrain\n" +
		"    each other are ONE question; say so.\n" +
		"  - A CODE FREEZE while the question is open, so the thread converges instead of\n" +
		"    racing your commits.\n" +
		"  - ONE THREAD for the discussion, linked every time you refer to it.\n" +
		"  When a reviewer ignores the protocol, say so directly, briefly and warmly.\n" +
		"  Lead with what their review got right, be specific about it, then name the\n" +
		"  miss with a number rather than a complaint. Ask once. Do not nag, do not\n" +
		"  moralise, and never trade quality for brevity: you want the same eyes and\n" +
		"  fewer words. A reviewer who found real defects has earned a light touch.\n"
}

func developCharge(w *Critic) string {
	var b strings.Builder
	b.WriteString("YOUR JOB — DEVELOP MODE\n")
	b.WriteString("  You are not primarily reviewing these PRs. You are the harness's own test:\n")
	b.WriteString("  every heartbeat, ebac hands you a claim about what changed, and your job is\n")
	b.WriteString("  to find out whether that claim is TRUE and to improve the system when it is not.\n\n")
	b.WriteString("  1. VALIDATE THE DELTA. Pick the claims above that are checkable and check them\n")
	b.WriteString("     against the live PR, read-only (gh pr view / gh api graphql). Confirm the\n")
	b.WriteString("     three failure shapes specifically:\n")
	b.WriteString("       - FALSE POSITIVE: an event reported that did not happen (or is our own echo).\n")
	b.WriteString("       - FALSE ABSENCE: something that DID change and is missing from the delta.\n")
	b.WriteString("         Check this one deliberately. It cannot be seen by reading the delta alone,\n")
	b.WriteString("         and it is the failure a quiet monitor is built to have.\n")
	b.WriteString("       - MISTIERED: a bot woke you, or a human's request landed in BACKGROUND.\n")
	b.WriteString("  2. RUN THE SELFCHECK. `" + fmt.Sprintf("ebac selfcheck --critic %s", w.Name) + "` asserts the\n")
	b.WriteString("     harness's invariants against live state. A WARN is a finding; read what it\n")
	b.WriteString("     measured before believing it, because the instrument can be wrong too.\n")
	b.WriteString("  3. IMPROVE ONE THING. The source is ~/dev/ebac. Make at most one focused\n")
	b.WriteString("     change per heartbeat, with a test that fails before it and passes after.\n")
	b.WriteString("     `go test ./...` must be green — gate on the command, not on a pipeline that\n")
	b.WriteString("     ends in tail or grep. Do not rewrite the design; fix what today measured.\n")
	if w.IssueRepo != "" {
		fmt.Fprintf(&b, "  4. FILE WHAT YOU CANNOT FIX. `ebac issue --critic %s --title ... --body-file ...`\n", w.Name)
		fmt.Fprintf(&b, "     opens an issue in %s. Use it for defects in the FORM or the HARNESS —\n", w.IssueRepo)
		b.WriteString("     never for opinions about the PRs under critic. One issue per defect, with the\n")
		b.WriteString("     round number, the command you ran, and what you expected versus what you saw.\n")
	}
	b.WriteString("\n  Report nothing you did not verify. An unreproduced suspicion is not a finding,\n")
	b.WriteString("  and 'looks fine' after an actual check is a valuable result worth one line.\n")
	return b.String()
}
