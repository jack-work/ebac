package main

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
	TierRecord Tier = 2 // written to state, never wakes the aria
	TierWake   Tier = 1 // worth a turn
)

type EventKind string

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
)

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
		fmt.Fprintf(&b, " — %s", firstLine(e.Detail))
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

// Diff computes prev -> next.
//
// Rules, each one paid for by a prior incident:
//   - A PR absent from `next` is NOT a deletion unless the scan was complete.
//   - A comment whose body digest moved is an edit, and an edit resets any
//     prior delivery state upstream (the poller handles that).
//   - Bot authorship demotes an event to tier 2 but never discards it.
func Diff(prev, next *Snapshot, me string) *Delta {
	d := &Delta{}
	if prev == nil {
		prev = &Snapshot{PRs: map[PRKey]*PRState{}, Complete: true}
	}

	for _, key := range next.SortedPRKeys() {
		np := next.PRs[key]
		pp := prev.PRs[key]

		if pp == nil {
			d.Events = append(d.Events, Event{
				Kind: EvPRAdded, Tier: TierWake, PR: key, URL: np.URL,
				Author: np.Author, Detail: np.Title,
			})
			// A newly-criticed PR reports its standing feedback as one
			// event, not one per thread: otherwise adding a busy PR
			// detonates a hundred-event first round.
			c := np.Counts(me)
			if c.Unresolved > 0 {
				d.Events = append(d.Events, Event{
					Kind: EvThreadNew, Tier: TierRecord, PR: key,
					Detail: fmt.Sprintf("%d pre-existing unresolved thread(s) at adoption", c.Unresolved),
				})
			}
			continue
		}

		diffPR(d, pp, np, me)
	}

	// Deletions, and the guard on them.
	for key, pp := range prev.PRs {
		if _, ok := next.PRs[key]; ok {
			continue
		}
		if !next.Complete {
			d.SuppressedDeletions = append(d.SuppressedDeletions, key)
			continue
		}
		_ = pp
		d.Events = append(d.Events, Event{
			Kind: EvPRClosed, Tier: TierRecord, PR: key,
			Detail: "no longer in critic scope",
		})
	}
	sort.SliceStable(d.SuppressedDeletions, func(i, j int) bool {
		return d.SuppressedDeletions[i] < d.SuppressedDeletions[j]
	})
	return d
}

func diffPR(d *Delta, pp, np *PRState, me string) {
	key := np.Key

	if pp.State != np.State {
		switch np.State {
		case "MERGED":
			d.Events = append(d.Events, Event{Kind: EvPRMerged, Tier: TierWake, PR: key, URL: np.URL})
		case "CLOSED":
			d.Events = append(d.Events, Event{Kind: EvPRClosed, Tier: TierWake, PR: key, URL: np.URL})
		case "OPEN":
			d.Events = append(d.Events, Event{Kind: EvPRReopened, Tier: TierWake, PR: key, URL: np.URL})
		}
	}

	if pp.IsDraft != np.IsDraft {
		detail := "marked ready for review"
		tier := TierWake
		if np.IsDraft {
			detail = "converted to draft"
			tier = TierRecord
		}
		d.Events = append(d.Events, Event{Kind: EvDraftChanged, Tier: tier, PR: key, Detail: detail})
	}

	if pp.HeadSHA != np.HeadSHA && np.HeadSHA != "" {
		// A push only earns a turn when there is outstanding feedback it
		// might have addressed. Force-push churn on a PR nobody has
		// commented on is not news.
		tier := TierRecord
		if c := np.Counts(me); c.Unresolved > 0 {
			tier = TierWake
		}
		d.Events = append(d.Events, Event{
			Kind: EvHeadMoved, Tier: tier, PR: key,
			Detail: short(pp.HeadSHA) + " -> " + short(np.HeadSHA),
		})
	}

	// Threads.
	for _, id := range np.SortedThreadIDs() {
		nt := np.Threads[id]
		pt := pp.Threads[id]

		if pt == nil {
			// Attribute the thread to whoever OPENED it, not whoever most
			// recently replied. Observed live in production use round
			// 567: bob raised a finding and alice replied within the
			// same poll gap, so this was the first time ebac ever saw the
			// thread -- with LastComment() it reported "thread_new by
			// alice" quoting alice's reply, crediting the operator's
			// own words as the finding and erasing bob's actual one.
			first := nt.FirstComment()
			ev := Event{
				Kind: EvThreadNew, Tier: TierWake, PR: key, ThreadID: id,
				Path: nt.Path,
			}
			if first != nil {
				ev.Author, ev.IsBot, ev.Comment, ev.URL = first.Author, first.IsBot, first.ID, first.URL
				ev.Detail = firstLine(first.Body)
				if first.IsBot {
					ev.Tier = TierRecord
				}
			}
			if nt.IsResolved {
				ev.Tier = TierRecord
			}
			d.Events = append(d.Events, ev)
			continue
		}

		if pt.IsResolved != nt.IsResolved {
			kind, tier := EvThreadResolved, TierRecord
			if !nt.IsResolved {
				// Somebody re-opened a thread: that is a request.
				kind, tier = EvThreadUnresolve, TierWake
			}
			d.Events = append(d.Events, Event{Kind: kind, Tier: tier, PR: key, ThreadID: id, Path: nt.Path})
		}

		prevComments := map[string]*Comment{}
		for _, c := range pt.Comments {
			prevComments[c.ID] = c
		}
		for _, nc := range nt.Comments {
			pc := prevComments[nc.ID]
			if pc == nil {
				ev := Event{
					Kind: EvCommentNew, Tier: TierWake, PR: key, ThreadID: id,
					Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
					Path: nt.Path, URL: nc.URL, Detail: firstLine(nc.Body),
				}
				// Our own words never wake us.
				if strings.EqualFold(nc.Author, me) || nc.IsBot || nt.IsResolved {
					ev.Tier = TierRecord
				}
				d.Events = append(d.Events, ev)
				continue
			}
			if pc.Digest != nc.Digest {
				ev := Event{
					Kind: EvCommentEdited, Tier: TierWake, PR: key, ThreadID: id,
					Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
					Path: nt.Path, URL: nc.URL, Detail: firstLine(nc.Body),
				}
				if strings.EqualFold(nc.Author, me) || nc.IsBot || nt.IsResolved {
					ev.Tier = TierRecord
				}
				d.Events = append(d.Events, ev)
			}
		}
	}

	// Thread disappearance, guarded by scan completeness.
	for id, pt := range pp.Threads {
		if _, ok := np.Threads[id]; ok {
			continue
		}
		if !np.Complete {
			d.SuppressedDeletions = append(d.SuppressedDeletions, key)
			continue
		}
		d.Events = append(d.Events, Event{
			Kind: EvThreadGone, Tier: TierRecord, PR: key, ThreadID: id, Path: pt.Path,
		})
	}

	// Reviews.
	//
	// A review's ID is stable across its lifetime, but its State is not: a
	// reviewer can leave a review PENDING (a draft, invisible to everyone
	// else) for days before clicking "submit", at which point the SAME
	// review ID flips to APPROVED / CHANGES_REQUESTED / COMMENTED. Keying
	// this loop purely on "is the ID new" -- as it did before -- means a
	// review adopted while PENDING can be submitted for real, days later,
	// and generate zero events forever: the ID was already "seen".
	for id, nr := range np.Reviews {
		pv, existed := pp.Reviews[id]
		if existed && pv.State == nr.State {
			if pv.Body == nr.Body {
				continue
			}
			// Same state, different body: some reviewer accounts edit
			// their verdict in place (observed live: an automated
			// "aether-review-bot:v1" identity holding one CHANGES_REQUESTED
			// review across many rounds while swapping the actual blocking
			// finding underneath). The label never changed but the
			// substance did -- that is exactly what a wake exists for.
			tier := TierWake
			if nr.IsBot || strings.EqualFold(nr.Author, me) {
				tier = TierRecord
			}
			d.Events = append(d.Events, Event{
				Kind: EvReviewEdited, Tier: tier, PR: key, Author: nr.Author,
				IsBot: nr.IsBot, Detail: nr.State + " (findings updated)", ReviewID: id,
			})
			continue
		}
		tier := TierWake
		if nr.IsBot || strings.EqualFold(nr.Author, me) || nr.State == "COMMENTED" && nr.Body == "" {
			tier = TierRecord
		}
		detail := nr.State
		if existed {
			detail = pv.State + " -> " + nr.State
		} else if nr.State == "DISMISSED" {
			// DISMISSED is not a state the GitHub API ever submits a review
			// into -- it is only reached by dismissing an existing APPROVED
			// or CHANGES_REQUESTED review. Seeing it here on a review ID we
			// have never seen before means the poll gap straddled the whole
			// approve-then-dismiss cycle: bare "DISMISSED" would claim a
			// submission that never happened and silently drop the real one
			// (an approval or changes-request) that did. Say so plainly
			// instead of asserting the fiction; the original verdict is
			// still readable in the review body if anyone needs it.
			detail = "DISMISSED (submitted and dismissed between polls; original verdict not observed)"
		}
		d.Events = append(d.Events, Event{
			Kind: EvReviewSubmitted, Tier: tier, PR: key, Author: nr.Author,
			IsBot: nr.IsBot, Detail: detail, ReviewID: id,
		})
	}

	// Top-level PR conversation comments: same new/edited shape as thread
	// comments, but with no ThreadID and no resolved-state demotion (a
	// top-level comment cannot be resolved).
	prevPRComments := map[string]*Comment{}
	for id, c := range pp.Comments {
		prevPRComments[id] = c
	}
	for _, id := range np.SortedCommentIDs() {
		nc := np.Comments[id]
		pc := prevPRComments[id]
		if pc == nil {
			ev := Event{
				Kind: EvPRCommentNew, Tier: TierWake, PR: key,
				Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
				URL: nc.URL, Detail: firstLine(nc.Body),
			}
			if strings.EqualFold(nc.Author, me) || nc.IsBot || isAutomatonAck(nc.Body) {
				ev.Tier = TierRecord
			}
			d.Events = append(d.Events, ev)
			continue
		}
		if pc.Digest != nc.Digest {
			ev := Event{
				Kind: EvPRCommentEdited, Tier: TierWake, PR: key,
				Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
				URL: nc.URL, Detail: firstLine(nc.Body),
			}
			if strings.EqualFold(nc.Author, me) || nc.IsBot || isAutomatonAck(nc.Body) {
				ev.Tier = TierRecord
			}
			d.Events = append(d.Events, ev)
		}
	}
}

// automatonAckMarker identifies a pure "you're in the queue" status ping
// from the Automaton review bot. Observed live in production use/#23654
// round 61: Automaton posts through a real, named human account ("bob" /
// Lukas Stankiewicz, GraphQL __typename "User", not "Bot") rather than a
// GitHub Bot identity, so isBot() -- which only ever looks at author login
// and __typename -- cannot catch it by identity. Content is the only signal
// available. The marker is distinct from the one on an actual verdict
// ("<!-- automaton:PR:SHA verdict:... -->", which DOES carry a reviewer's
// real findings and must keep waking); this one never does.
const automatonAckMarker = "<!-- automaton-ack:"

func isAutomatonAck(body string) bool {
	return strings.Contains(body, automatonAckMarker)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 110
	if len(s) > max {
		// Trim on a rune boundary so a multi-byte character is never cut
		// in half into a replacement char.
		r := []rune(s)
		if len(r) > max {
			r = r[:max]
		}
		s = string(r) + "…"
	}
	return s
}
