package model

import (
	"fmt"
	"sort"
	"strings"
)

// Tier decides whether an event is worth a Figaro turn.
//
// A review costs real money (the prangl log measures one at ~2.8M billable
// tokens, and identifies TURNS as the lever, not verbosity). So the default
// is silence: an event must earn tier 1 to wake anyone.
type Tier int

const (
	EvPRAdded         EventKind = "pr_added"
	EvPRClosed        EventKind = "pr_closed"
	EvPRMerged        EventKind = "pr_merged"
	EvPRReopened      EventKind = "pr_reopened"
	EvHeadMoved       EventKind = "head_moved"
	EvDraftChanged    EventKind = "draft_changed"
	EvThreadNew       EventKind = "thread_new"
	EvThreadResolved  EventKind = "thread_resolved"
	EvThreadUnresolve EventKind = "thread_unresolved"
	EvThreadGone      EventKind = "thread_deleted"
	EvCommentNew      EventKind = "comment_new"
	EvCommentEdited   EventKind = "comment_edited"
	EvReviewSubmitted EventKind = "review_submitted"
	// EvReviewEdited fires when a review's State did NOT change but its
	// Body did: some reviewer accounts (observed live: an automated
	// "aether-review-bot:v1" identity) edit their verdict in place as new
	// commits land, holding one review ID CHANGES_REQUESTED for the whole
	// PR while silently swapping which finding is the blocking one. A
	// diff keyed on State alone sees nothing there to report.
	EvReviewEdited EventKind = "review_edited"
	// EvPRCommentNew/Edited cover top-level PR conversation comments —
	// GraphQL pullRequest.comments, not tied to any review thread. Kept as
	// distinct kinds from EvCommentNew/Edited (which are thread-scoped)
	// because a top-level comment has no ThreadID and no resolution state.
	EvPRCommentNew    EventKind = "pr_comment_new"
	EvPRCommentEdited EventKind = "pr_comment_edited"
	// EvPendingReview: an unsubmitted review of ours exists. Every thread
	// reply lands in it unseen and every new review is refused until the
	// operator submits or deletes it; ebac does neither.
	EvPendingReview     EventKind = "pending_review"
	EvPendingReviewGone EventKind = "pending_review_gone"
)

const (
	TierRecord Tier = 2 // written to state, never wakes the aria
	TierWake   Tier = 1 // worth a turn
)

type EventKind string

type Event struct {
	Kind     EventKind `json:"kind"`
	Tier     Tier      `json:"tier"`
	PR       PRKey     `json:"pr"`
	ThreadID string    `json:"thread_id,omitempty"`
	Comment  string    `json:"comment_id,omitempty"`
	// ReviewID is the GraphQL review node ID for EvReviewSubmitted. It
	// exists only so mergeEvents' dedup key has something to distinguish
	// one review submission from another: two reviews on the same PR by
	// the same author in the same state (e.g. GitHub splitting one
	// "batch" of inline comments into several COMMENTED reviews) are
	// otherwise byte-for-byte identical Events and collapse to one.
	ReviewID string `json:"review_id,omitempty"`
	Author   string `json:"author,omitempty"`
	IsBot    bool   `json:"is_bot,omitempty"`
	Stamped  bool   `json:"stamped,omitempty"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (e Event) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", e.PR, e.Kind)
	if e.Author != "" {
		who := e.Author
		if e.IsBot {
			who += " (bot)"
		}
		if e.Stamped {
			who += " (bot-stamped, not a person's review)"
		}
		fmt.Fprintf(&b, " by %s", who)
	}
	if e.Path != "" {
		fmt.Fprintf(&b, " on %s", e.Path)
	}
	if e.Detail != "" {
		// Cap here, not only at the call sites that build Detail. This
		// string lands in a form that is mirrored whole into a model's
		// context; a bound that depends on every producer remembering to
		// apply it is not a bound.
		fmt.Fprintf(&b, " — %s", FirstLine(e.Detail))
	}
	return b.String()
}

type Delta struct {
	Events []Event `json:"events"`
	// SuppressedDeletions records that a partial scan prevented us from
	// concluding anything was removed. It is a first-class result, not a
	// silence: "the instrument could not see" and "nothing was there" must
	// never render identically.
	SuppressedDeletions []PRKey `json:"suppressed_deletions,omitempty"`
}

func (d *Delta) Empty() bool { return len(d.Events) == 0 }

func (d *Delta) Tier1() []Event {
	var out []Event
	for _, e := range d.Events {
		if e.Tier == TierWake {
			out = append(out, e)
		}
	}
	return out
}

func (d *Delta) WakeWorthy() bool { return len(d.Tier1()) > 0 }

// Summary is the one line that lands on the form. Keep it short: the form
// is mirrored whole into context on every change.
func (d *Delta) Summary() string {
	if d.Empty() {
		return "no change"
	}
	byKind := map[EventKind]int{}
	order := []EventKind{}
	for _, e := range d.Tier1() {
		if byKind[e.Kind] == 0 {
			order = append(order, e.Kind)
		}
		byKind[e.Kind]++
	}
	if len(order) == 0 {
		return fmt.Sprintf("%d background event(s), none wake-worthy", len(d.Events))
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%d %s", byKind[k], k))
	}
	return strings.Join(parts, ", ")
}
