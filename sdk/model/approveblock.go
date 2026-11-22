package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The approve blocker rule: an
// approval goes out only at a head where nothing blocks it. A standing
// REQUEST_CHANGES (an author's newest APPROVED, CHANGES_REQUESTED or
// DISMISSED review is CHANGES_REQUESTED: GitHub keeps it in force across
// pushes until it is dismissed or superseded) blocks, and so does an
// unresolved thread a comment marks blocking, UNLESS that review's or
// comment's body carries one of the critic's trusted automated markers
// (Critic.AutomatedMarkers: data in ebac's trusted config, never in code).
// No markers: every such review and thread blocks. A PR not read whole
// blocks (a review or thread past the read could be the blocker).

var (
	notBlockingRe = regexp.MustCompile(`(?i)\b(?:not|non)[- ]?(?:a[- ])?block(?:ing|er)\b`)
	blockingRe    = regexp.MustCompile(`(?i)\bblock(?:ing|er)\b`)
)

// ApproveBlocked prefixes the refusal of an approval the blocker rule stops.
const ApproveBlocked = "approval blocked at this head: "

// MarksBlocking: a comment body that calls its thread blocking ("blocking",
// "blocker"; "not blocking", "non-blocking", "not a blocker" do not).
func MarksBlocking(body string) bool {
	return blockingRe.MatchString(notBlockingRe.ReplaceAllString(body, ""))
}

// automated: body carries one of the markers (an empty marker matches
// nothing: it would exempt every body).
func automated(body string, markers []string) bool {
	for _, m := range markers {
		if m != "" && strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// ApproveBlockers are why an approval of pr at its head must not go out, in
// a stable order; none means it may.
func ApproveBlockers(pr *PRState, markers []string) []string {
	var out []string
	if !pr.Complete {
		out = append(out, "the PR was not read whole this round (reviews or threads past the read could block)")
	}
	newest := map[string]*Review{}
	for _, r := range pr.Reviews {
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		who := strings.ToLower(r.Author)
		if p := newest[who]; p == nil || r.SubmittedAt > p.SubmittedAt || (r.SubmittedAt == p.SubmittedAt && r.ID > p.ID) {
			newest[who] = r
		}
	}
	var rs []string
	for who, r := range newest {
		if r.State == "CHANGES_REQUESTED" && !automated(r.Body, markers) {
			rs = append(rs, fmt.Sprintf("a REQUEST_CHANGES by %s (review %s) carries no automated marker", who, r.ID))
		}
	}
	sort.Strings(rs)
	out = append(out, rs...)
	var ts []string
	for _, t := range pr.Threads {
		if t.IsResolved {
			continue
		}
		for _, c := range t.Comments {
			if MarksBlocking(c.Body) && !automated(c.Body, markers) {
				ts = append(ts, fmt.Sprintf("unresolved thread %s (%s) is marked blocking by %s with no automated marker", t.ID, t.Path, c.Author))
				break
			}
		}
	}
	sort.Strings(ts)
	return append(out, ts...)
}
