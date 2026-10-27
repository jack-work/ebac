package brief

import (
	"fmt"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// What a seat sees of its form (figaro 0.48, measured 2026-10-20): each
// studied change arrives as a study:@<form> block holding only the LEAF paths
// that moved, so one new comment is one key. Each leaf is cut at the
// studier's system.delta_key_bytes (default 2048) and a block at
// system.delta_bytes (default 8192), with a note naming how to read the rest.
// The harness contract tells the seat to read the form directly when it sees
// a cut.

func DescribeStop(s model.StopConditions) string {
	var parts []string
	if s.UntilAllClosed {
		g := time.Duration(s.ArchiveGraceSec) * time.Second
		parts = append(parts, fmt.Sprintf("all PRs closed/merged + %s grace", g))
	}
	if s.MaxRounds > 0 {
		parts = append(parts, fmt.Sprintf("%d rounds", s.MaxRounds))
	}
	if s.Deadline != nil {
		parts = append(parts, "deadline "+s.Deadline.UTC().Format(time.RFC3339))
	}
	if s.MaxConsecutiveFailures > 0 {
		parts = append(parts, fmt.Sprintf("%d consecutive failures", s.MaxConsecutiveFailures))
	}
	if len(parts) == 0 {
		return "never (manual stop only)"
	}
	return strings.Join(parts, " | ")
}

func tier2(events []model.Event) []model.Event {
	var out []model.Event
	for _, e := range events {
		if e.Tier != model.TierWake {
			out = append(out, e)
		}
	}
	return out
}

// harnessContract rides on the form so the seat is never guessing at the
// protocol, and so the dotted-key hazard is stated where it will be read.
// formValueElisionLimit is where a studied form's value is cut. MEASURED,
// not documented — the docs claim values are "not truncated", and they are:
// 2046 characters survive, 2047 is elided with a trailing `…`.
const FormValueElisionLimit = 2046

type HarnessContract struct {
	Truncation string `json:"if_a_value_looks_cut"`
	Rule       string `json:"key_writing_rule"`
	Ack        string `json:"ack_when_done"`
	Selfcheck  string `json:"selfcheck"`
	Issue      string `json:"file_harness_bug"`
	Authority  string `json:"authority"`
	Post       string `json:"post"`
	Protocol   string `json:"protocol"`
}

func NewHarnessContract(w *model.Critic) HarnessContract {
	c := HarnessContract{
		Truncation: fmt.Sprintf("Values here are cut at %d chars with a trailing … (a cut object also has unbalanced braces). Expected, not a bug. On either sign read the real value with `figaro form %s -j` before drawing a conclusion, and never report on an elided value.", FormValueElisionLimit, w.FormID),
		Rule:       "write ONLY ack_round, ack_note and post.<id>. ebac owns kind, critic, prs, harness and harness_protocol_*: anything written there is overwritten next round.",
		Post:       fmt.Sprintf("Post through this form, never with gh directly: `ebac post --critic %s --pr <owner/repo#n> [--reply-to <thread id> [--react +1] [--resolve] | --path <file> --line <n>] [--event COMMENT|APPROVE] --body-file <file>`. --react reacts to the comment that opened the thread; --resolve resolves it after the reply; with either, --body-file is optional. A top-level PR comment omits --event; --event submits a review, and so does --path. ebac sends it within one round and writes post.<id>.sent (url), .reacted, .resolved, .refused (why) or .error. A pending (unsubmitted) review of yours on the PR swallows replies unseen and blocks reviews: ebac refuses those until the operator submits or deletes it.", w.Name),
		Ack:        fmt.Sprintf("ebac ack --critic %s --round <delta.round> [--note '<one line>']", w.Name),
		Selfcheck:  fmt.Sprintf("ebac selfcheck --critic %s", w.Name),
		Authority:  "READ-ONLY on the pull requests: never comment, review, approve, push, label or merge. Report to the operator instead.",
		Protocol:   "Your standing protocol is keys " + ProtocolKeyPrefix + "1, 2, ... read in order. Deliveries carry only the delta and the ack command.",
	}
	switch {
	case w.Mode == model.ModeReviewer && w.Approve:
		c.Authority = "REVIEWER: comment, reply, and approve PRs you did not author. Never request changes, push, label, merge or close."
	case w.Mode == model.ModeReviewer:
		c.Authority = "REVIEWER: comment and reply. Never approve, request changes, push, label, merge or close."
	case w.Write:
		c.Authority = "WRITE granted on this critic's pull requests: reply, comment and push to the head branch are allowed. Approve, merge and close are NOT. Any PR not named on this critic stays read-only."
	}
	if w.IssueRepo != "" {
		c.Issue = fmt.Sprintf("ebac issue --critic %s --title '<title>' --body-file <path>   (files into %s)", w.Name, w.IssueRepo)
	} else {
		c.Issue = "no issue repo configured; run: ebac set-issue-repo --critic " + w.Name + " --repo <owner/repo>"
	}
	return c
}
