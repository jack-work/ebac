package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The brief is what actually costs money: it is a turn. The round's change is
// already on the role form, which the seat studies, so the brief only says
// where to look: the summary, the standing, the form, and the ack.
func buildBrief(w *Critic, snap *Snapshot, events []Event, me string, retry int) string {
	d := &Delta{Events: events}
	var b strings.Builder

	fmt.Fprintf(&b, "ebac %s round %d: %s\n", w.Name, w.Runtime.Round, d.Summary())
	if retry > 0 {
		fmt.Fprintf(&b, "(redelivery attempt %d; earlier sends failed, so this batch may repeat news you have)\n", retry+1)
	}
	if snap != nil {
		for _, k := range snap.SortedPRKeys() {
			p := snap.PRs[k]
			c := p.Counts(me)
			flag := ""
			if !p.Complete {
				flag = " [scan incomplete]"
			}
			fmt.Fprintf(&b, "  %s %s head %s: %d unresolved, %d awaiting a reply%s\n",
				k, p.State, short(p.HeadSHA), c.Unresolved, c.AwaitingUs, flag)
		}
	}
	fmt.Fprintf(&b, "The comments are in your studied form %s (keys prs.*): read them in its diff, in full. "+
		"Also `figaro form %s -j`, or GitHub. Not studying %s is a bug: `figaro study %s`.\n",
		w.FormID, w.FormID, w.FormID, w.FormID)
	fmt.Fprintf(&b, "Post through the form (harness.post); protocol in harness_protocol_*. "+
		"When finished: ebac ack --critic %s --round %d [--note '<one line>']\n", w.Name, w.Runtime.Round)
	return b.String()
}

// protocolText is everything a seat must know that does not change between
// rounds. It is projected onto the role form, never repeated in a brief.
func protocolText(w *Critic, snap *Snapshot, me string) string {
	var b strings.Builder
	b.WriteString("BOUNDARY\n")
	if w.Mode == ModeReviewer {
		b.WriteString(reviewerBoundary(w, snap, me))
	} else if w.Write {
		b.WriteString("  You have WRITE on these pull requests: reply to threads, comment, and push\n")
		b.WriteString("  to the head branch. The operator granted this per-critic. You may NOT approve,\n")
		b.WriteString("  merge or close — those are irreversible and are not yours. Act only on the PRs\n")
		b.WriteString("  named on this critic; any other PR is still read-only.\n\n")
	} else {
		b.WriteString("  You are READ-ONLY on these pull requests. Do not comment, review, approve,\n")
		b.WriteString("  request changes, push, label, close or merge. The operator did not ask you to\n")
		b.WriteString("  touch their PRs and you have no gate. Report; do not act.\n\n")
	}
	switch w.Mode {
	case ModeDevelop:
		b.WriteString(developCharge(w))
	case ModeReviewer:
		b.WriteString(reviewerCharge(w, snap))
	default:
		b.WriteString(reviewCharge(w))
	}
	b.WriteString("\nACK every delivery with the command it names, even when you decided to do nothing:\n")
	b.WriteString("acking is what stops that delta being re-raised.\n")
	return b.String()
}

// protocolChunkLimit keeps each chunk's JSON under the study elision cut.
const protocolChunkLimit = formValueElisionLimit - 46

// protocolChunks splits text on line boundaries so no chunk, JSON-encoded,
// is cut when a seat studies the form. Concatenated, the chunks are the text.
func protocolChunks(text string) []string {
	var out []string
	var cur strings.Builder
	fits := func(s string) bool { return jsonLen(s) <= protocolChunkLimit }
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if cur.Len() > 0 && !fits(cur.String()+line) {
			out = append(out, cur.String())
			cur.Reset()
		}
		if !fits(line) {
			for _, r := range line {
				if cur.Len() > 0 && !fits(cur.String()+string(r)) {
					out = append(out, cur.String())
					cur.Reset()
				}
				cur.WriteRune(r)
			}
			continue
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func jsonLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b)
}

func reviewCharge(w *Critic) string {
	var b strings.Builder
	b.WriteString("YOUR JOB\n")
	b.WriteString("  Read the delta. Decide whether anything here needs the operator. If it does, say so\n")
	b.WriteString("  in one or two sentences with the URL. If it does not, ack and stay silent.\n")
	b.WriteString("  A quiet round is a correct round.\n")
	if w.Write {
		b.WriteString(writeDiscipline())
		b.WriteString(signalDiscipline())
	}
	return b.String()
}

// signalDiscipline is how a seat that may post talks on a PR. Agents read these
// threads and the operator skims them, so every post is a signal, not prose.
func signalDiscipline() string {
	return "\n" +
		"SIGNAL, DO NOT NARRATE\n" +
		"  Agents read this PR; the operator skims it. Aim for extreme information density.\n" +
		"  - SILENCE IS A REPLY. Post only what changes another agent's next action.\n" +
		"  - A REACTION IS THE RECEIPT. A finding fixed at the head: resolve the thread,\n" +
		"    `ebac post ... --reply-to <thread> --resolve`, with --body-file only when the fix\n" +
		"    sha or a reason is not obvious. A reply without --resolve leaves the thread open.\n" +
		"  - ONE LINE PER ANSWER: verdict, then sha, file:line or test name, or the reason.\n" +
		"    No thanks, no recap, no restating the finding, no em dashes, never repeat yourself.\n" +
		"  - TOO MUCH PRESSURE ON ONE SECTION, or a loop: one line asking that reviewer to\n" +
		"    await the operator's signal on it, then escalate to the operator.\n" +
		"  - A COMMENT FOUND INACCURATE IS DELETED, NOT REWORDED. No comment beats most\n" +
		"    comments; the code and the commit message keep the record.\n" +
		"  - KNOW THE CHANGE. Accept a finding because you verified it, never because an\n" +
		"    agent asked. A steady stream of valid findings is taken; churn is stopped.\n"
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

// signaturePath is $EBAC_SIGNATURE, else $XDG_CONFIG_HOME/ebac/signature.
func signaturePath() string {
	if p := os.Getenv("EBAC_SIGNATURE"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "ebac", "signature")
}

func readSignature() (string, error) {
	path := signaturePath()
	raw, err := os.ReadFile(path)
	sig := strings.TrimSpace(string(raw))
	if err != nil || sig == "" {
		return "", fmt.Errorf("reviewer mode posts under a signature; write one to %s", path)
	}
	return sig, nil
}

func reviewerBoundary(w *Critic, snap *Snapshot, me string) string {
	var b strings.Builder
	b.WriteString("  You may comment on and reply to threads on the pull requests above.\n")
	if w.Approve {
		var ok []string
		if snap != nil {
			for _, k := range snap.SortedPRKeys() {
				if p := snap.PRs[k]; !strings.EqualFold(p.Author, me) {
					ok = append(ok, string(k))
				}
			}
		}
		fmt.Fprintf(&b, "  You may APPROVE: %s.\n", strings.Join(ok, ", "))
	}
	b.WriteString("  Never request changes, push, label, merge or close.\n\n")
	return b.String()
}

func reviewerCharge(w *Critic, snap *Snapshot) string {
	var b strings.Builder
	b.WriteString("YOUR JOB — REVIEWER\n")
	b.WriteString("  Review someone else's change with a light touch. The author decides everything.\n")
	b.WriteString("  1. ORIENT. State the PR's intent to yourself in one sentence. A finding that\n")
	b.WriteString("     fights the intent is scope, not a defect: drop it.\n")
	if w.Notes != "" {
		fmt.Fprintf(&b, "     Read %s first: this repo's conventions and how to validate locally.\n", w.Notes)
	}
	if wt := w.Worktree; wt != nil && snap != nil {
		b.WriteString("  2. CHECK OUT THE HEAD and work only there:\n")
		for _, k := range snap.SortedPRKeys() {
			p := snap.PRs[k]
			dir := filepath.Join(wt.Root, fmt.Sprintf("%s-pr%d", w.Name, p.Number))
			fmt.Fprintf(&b, "       git -C %s fetch origin pull/%d/head && git -C %s worktree add --detach %s FETCH_HEAD\n",
				wt.Clone, p.Number, wt.Clone, dir)
			fmt.Fprintf(&b, "     new head: git -C %s fetch origin pull/%d/head && git -C %s checkout --detach FETCH_HEAD\n",
				dir, p.Number, dir)
		}
		fmt.Fprintf(&b, "     When the PR closes: git -C %s worktree remove --force <dir>\n", wt.Clone)
	} else {
		b.WriteString("  2. CHECK OUT THE HEAD in a scratch worktree of your own, never a shared tree.\n")
	}
	b.WriteString("  3. VERIFY OR DROP. Post only what you ran: a failing test, a command, a repro at\n")
	b.WriteString("     the head. Apply every suggested fix in the worktree and run its tests first.\n")
	b.WriteString("     Drop the unverified silently; never soften it into a nit.\n")
	b.WriteString("  4. BEYOND CORRECTNESS, watch for two things:\n")
	b.WriteString("     - Inline justification: comments that narrate the next line, carry history\n")
	b.WriteString("       (dates, incidents, \"previously\", review back-and-forth) or repeat what a doc,\n")
	b.WriteString("       README or the PR body already says. They go stale. Suggest deleting, or one\n")
	b.WriteString("       sentence on the doc comment of the function, type or module.\n")
	b.WriteString("     - Imperative tests: cases that share setup and differ only in data belong in a\n")
	b.WriteString("       table or parametrised fixture with literal expected values. A test of its own\n")
	b.WriteString("       mocks, of the framework, or duplicating another may need not exist. Never\n")
	b.WriteString("       suggest removing the only test that fails when the change is reverted; show a\n")
	b.WriteString("       rewrite green at the head and red with the change reverted before suggesting it.\n")
	b.WriteString("  5. NEVER NITPICK. No style, naming, formatting, lint-catchable or pre-existing\n")
	b.WriteString("     issues; no \"consider adding tests\" without the exact test; never re-raise a\n")
	b.WriteString("     point the author answered. One root cause at several sites is one comment.\n")
	b.WriteString("  6. POST ONE REVIEW PER HEAD, through the form:\n")
	fmt.Fprintf(&b, "       ebac post --critic %s --pr <owner/repo#n> --event COMMENT|APPROVE --body-file <file>\n", w.Name)
	fmt.Fprintf(&b, "     and an inline finding: ebac post --critic %s --pr <pr> --path <file> --line <n> --body-file <file>\n", w.Name)
	b.WriteString("     Usually zero or one inline comment, never more than three: one or two sentences, then\n")
	b.WriteString("     optionally a ```suggestion block of a few lines that you applied and tested. The review\n")
	b.WriteString("     body is one line, then the SIGNATURE below verbatim. Name no agent, session or local path.\n")
	if w.Approve {
		b.WriteString("  7. APPROVE unless a VERIFIED finding is a correctness, security or data-loss\n")
		b.WriteString("     defect; then COMMENT and tell the operator in one line. A push may dismiss\n")
		b.WriteString("     your approval: on a new head, review only the new commits and approve again.\n")
		b.WriteString("     A draft cannot be approved: comment now; leaving draft wakes you to approve.\n")
	} else {
		b.WriteString("  7. Nothing survived verification? Post nothing. That is a successful review.\n")
	}
	b.WriteString("  8. REPLIES. Concede fast when the author is right; one line when not. Never\n")
	b.WriteString("     resolve someone else's thread.\n")
	b.WriteString(signalDiscipline())
	if sig, err := readSignature(); err == nil {
		b.WriteString("\nSIGNATURE\n")
		b.WriteString(sig)
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "\nNO SIGNATURE: %v. Post nothing until the operator fixes it.\n", err)
	}
	return b.String()
}
