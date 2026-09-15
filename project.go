package main

import (
	"fmt"
	"strings"
	"time"
)

// The form is a MAILBOX, not a database.
//
// MEASURED 2026-08-26, because the docs say otherwise and the docs are wrong:
//
//  1. A studied form is NOT re-sent whole on every change. Each `study:@x`
//     block carries only the keys that moved. (Changing a 28-char key cost
//     +110 cache-write tokens while a 20KB key sat untouched in the same
//     form; changing that 20KB key cost +2139.)
//  2. Each value IS truncated, at 2046 characters, with a trailing `…`.
//     Boundary measured exactly: 2046 visible, 2047 elided.
//
// So bodies still live in the on-disk snapshot and the form still carries
// counts and one-liners — but the reason is DATA LOSS, not token cost. A
// `prs` object over 2046 characters is silently cut mid-string.
//
// That is tolerable because it is DETECTABLE: a cut string ends in `…`, and
// a cut object is unbalanced and unparseable. The harness contract tells
// every seat to re-read the form directly when it sees either.

const maxProjectedEvents = 12

type PRProjection struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	State      string `json:"state"`
	Draft      bool   `json:"draft,omitempty"`
	Head       string `json:"head"`
	Threads    int    `json:"threads"`
	Unresolved int    `json:"unresolved"`
	AwaitingUs int    `json:"awaiting_reply"`
	Updated    string `json:"updated"`
	Incomplete bool   `json:"scan_incomplete,omitempty"`
}

type SyncProjection struct {
	Round               int    `json:"round"`
	LastPolled          string `json:"last_polled"`
	LastSuccess         string `json:"last_success,omitempty"`
	OK                  bool   `json:"ok"`
	Error               string `json:"error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	ScanComplete        bool   `json:"scan_complete"`
	SuppressedDeletions int    `json:"suppressed_deletions,omitempty"`
	Stopped             bool   `json:"stopped,omitempty"`
	StopReason          string `json:"stop_reason,omitempty"`
}

type DeltaProjection struct {
	Round   int      `json:"round"`
	Pending bool     `json:"pending"`
	Wake    bool     `json:"wake_worthy"`
	Count   int      `json:"count"`
	Tier1   int      `json:"tier1"`
	Summary string   `json:"summary"`
	Since   string   `json:"since,omitempty"`
	Events  []string `json:"events,omitempty"`
	Elided  int      `json:"elided,omitempty"`
	Retries int      `json:"delivery_retries,omitempty"`
}

type CriticProjection struct {
	Name      string   `json:"name"`
	Mode      string   `json:"mode"`
	Host      string   `json:"host"`
	PRs       []string `json:"prs"`
	Created   string   `json:"created"`
	IssueRepo string   `json:"issue_repo,omitempty"`
	Discover  string   `json:"discover,omitempty"`
	StopWhen  string   `json:"stop_when"`
}

func projectCritic(w *Critic) CriticProjection {
	keys := make([]string, 0, len(w.PRs))
	for _, r := range w.PRs {
		keys = append(keys, string(r.Key()))
	}
	p := CriticProjection{
		Name: w.Name, Mode: string(w.Mode), Host: w.Host, PRs: keys,
		Created: w.CreatedAt.UTC().Format(time.RFC3339), IssueRepo: w.IssueRepo,
		StopWhen: describeStop(w.Stop),
	}
	if w.DiscoverRepo != "" {
		p.Discover = w.DiscoverRepo
		if w.DiscoverAuthor != "" {
			p.Discover += " author:" + w.DiscoverAuthor
		}
	}
	return p
}

func describeStop(s StopConditions) string {
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

func projectPRs(snap *Snapshot, me string) map[string]PRProjection {
	out := map[string]PRProjection{}
	if snap == nil {
		return out
	}
	for _, k := range snap.SortedPRKeys() {
		p := snap.PRs[k]
		c := p.Counts(me)
		out[string(k)] = PRProjection{
			URL: p.URL, Title: firstLine(p.Title), State: p.State, Draft: p.IsDraft,
			Head: short(p.HeadSHA), Threads: c.Threads, Unresolved: c.Unresolved,
			AwaitingUs: c.AwaitingUs, Updated: p.UpdatedAt, Incomplete: !p.Complete,
		}
	}
	return out
}

func projectDelta(round int, events []Event, pending bool, since time.Time, retries int) DeltaProjection {
	d := &Delta{Events: events}
	t1 := d.Tier1()
	p := DeltaProjection{
		Round: round, Pending: pending, Wake: len(t1) > 0, Count: len(events),
		Tier1: len(t1), Summary: d.Summary(), Retries: retries,
	}
	if !since.IsZero() {
		p.Since = since.UTC().Format(time.RFC3339)
	}
	// Tier-1 lines first: if anything is elided it should be the noise.
	ordered := append(append([]Event{}, t1...), tier2(events)...)
	for i, e := range ordered {
		if i >= maxProjectedEvents {
			p.Elided = len(ordered) - maxProjectedEvents
			break
		}
		p.Events = append(p.Events, e.Line())
	}
	return p
}

func tier2(events []Event) []Event {
	var out []Event
	for _, e := range events {
		if e.Tier != TierWake {
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
const formValueElisionLimit = 2046

type HarnessContract struct {
	Truncation string `json:"if_a_value_looks_cut"`
	Rule       string `json:"key_writing_rule"`
	Ack        string `json:"ack_when_done"`
	Selfcheck  string `json:"selfcheck"`
	Issue      string `json:"file_harness_bug"`
	ReadOnly   string `json:"read_only"`
}

func harnessContract(w *Critic) HarnessContract {
	c := HarnessContract{
		Truncation: fmt.Sprintf("Values here are cut at %d chars with a trailing … (a cut object also has unbalanced braces). Expected, not a bug. On either sign read the real value with `figaro form %s -j` before drawing a conclusion, and never report on an elided value.", formValueElisionLimit, w.FormID),
		Rule:       "write ONLY flat scalar keys (ack_round, ack_note). A dotted key inside prs/delta/sync creates a flat decoy that shadows nothing.",
		Ack:        fmt.Sprintf("ebac ack --critic %s --round <delta.round> [--note '<one line>']", w.Name),
		Selfcheck:  fmt.Sprintf("ebac selfcheck --critic %s", w.Name),
		ReadOnly:   "READ-ONLY on the pull requests: never comment, review, approve, push, label or merge. Report to the operator instead.",
	}
	if w.Write {
		c.ReadOnly = "WRITE granted on this critic's pull requests: reply, comment and push to the head branch are allowed. Approve, merge and close are NOT. Any PR not named on this critic stays read-only."
	}
	if w.IssueRepo != "" {
		c.Issue = fmt.Sprintf("ebac issue --critic %s --title '<title>' --body-file <path>   (files into %s)", w.Name, w.IssueRepo)
	} else {
		c.Issue = "no issue repo configured; run: ebac set-issue-repo --critic " + w.Name + " --repo <owner/repo>"
	}
	return c
}
