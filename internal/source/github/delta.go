package github

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// Diff computes prev -> next.
//
// Rules, each one paid for by a prior incident:
//   - A PR absent from `next` is NOT a deletion unless the scan was complete.
//   - A comment whose body digest moved is an edit, and an edit resets any
//     prior delivery state upstream (the poller handles that).
//   - Bot authorship demotes an event to tier 2 but never discards it.
func Diff(prev, next *model.Snapshot, me string) *model.Delta {
	d := &model.Delta{}
	if prev == nil {
		prev = &model.Snapshot{PRs: map[model.PRKey]*model.PRState{}, Complete: true}
	}

	for _, key := range next.SortedPRKeys() {
		np := next.PRs[key]
		pp := prev.PRs[key]

		if pp == nil {
			d.Events = append(d.Events, model.Event{
				Kind: model.EvPRAdded, Tier: model.TierWake, PR: key, URL: np.URL,
				Author: np.Author, Detail: np.Title,
			})
			// A newly-criticed PR reports its standing feedback as one
			// event, not one per thread: otherwise adding a busy PR
			// detonates a hundred-event first round.
			c := np.Counts(me)
			if c.Unresolved > 0 {
				d.Events = append(d.Events, model.Event{
					Kind: model.EvThreadNew, Tier: model.TierRecord, PR: key,
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
		d.Events = append(d.Events, model.Event{
			Kind: model.EvPRClosed, Tier: model.TierRecord, PR: key,
			Detail: "no longer in critic scope",
		})
	}
	sort.SliceStable(d.SuppressedDeletions, func(i, j int) bool {
		return d.SuppressedDeletions[i] < d.SuppressedDeletions[j]
	})
	return d
}

func diffPR(d *model.Delta, pp, np *model.PRState, me string) {
	key := np.Key

	if pp.State != np.State {
		switch np.State {
		case "MERGED":
			d.Events = append(d.Events, model.Event{Kind: model.EvPRMerged, Tier: model.TierWake, PR: key, URL: np.URL})
		case "CLOSED":
			d.Events = append(d.Events, model.Event{Kind: model.EvPRClosed, Tier: model.TierWake, PR: key, URL: np.URL})
		case "OPEN":
			d.Events = append(d.Events, model.Event{Kind: model.EvPRReopened, Tier: model.TierWake, PR: key, URL: np.URL})
		}
	}

	if pp.IsDraft != np.IsDraft {
		detail := "marked ready for review"
		tier := model.TierWake
		if np.IsDraft {
			detail = "converted to draft"
			tier = model.TierRecord
		}
		d.Events = append(d.Events, model.Event{Kind: model.EvDraftChanged, Tier: tier, PR: key, Detail: detail})
	}

	had := map[string]bool{}
	for _, r := range pp.Pending {
		had[r.ID] = true
	}
	for _, r := range np.Pending {
		if !had[r.ID] {
			d.Events = append(d.Events, model.Event{Kind: model.EvPendingReview, Tier: model.TierWake, PR: key, ReviewID: r.ID, Author: r.Author,
				Detail: fmt.Sprintf("review %d (opened %s) holds %d comments only its author can see; replies land in it and new reviews are refused until the operator submits or deletes it",
					r.DatabaseID, r.CreatedAt, r.Comments)})
		}
		delete(had, r.ID)
	}
	for id := range had {
		d.Events = append(d.Events, model.Event{Kind: model.EvPendingReviewGone, Tier: model.TierRecord, PR: key, ReviewID: id,
			Detail: "the pending review was submitted or deleted"})
	}

	if pp.HeadSHA != np.HeadSHA && np.HeadSHA != "" {
		// A push only earns a turn when there is outstanding feedback it
		// might have addressed. Force-push churn on a PR nobody has
		// commented on is not news.
		tier := model.TierRecord
		if c := np.Counts(me); c.Unresolved > 0 {
			tier = model.TierWake
		}
		d.Events = append(d.Events, model.Event{
			Kind: model.EvHeadMoved, Tier: tier, PR: key,
			Detail: model.Short(pp.HeadSHA) + " -> " + model.Short(np.HeadSHA),
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
			ev := model.Event{
				Kind: model.EvThreadNew, Tier: model.TierWake, PR: key, ThreadID: id,
				Path: nt.Path,
			}
			if first != nil {
				ev.Author, ev.IsBot, ev.Comment, ev.URL = first.Author, first.IsBot, first.ID, first.URL
				ev.Detail = model.FirstLine(first.Body)
				if first.IsBot || strings.EqualFold(first.Author, me) {
					ev.Tier = model.TierRecord
				}
			}
			if nt.IsResolved {
				ev.Tier = model.TierRecord
			}
			d.Events = append(d.Events, ev)
			continue
		}

		if pt.IsResolved != nt.IsResolved {
			kind, tier := model.EvThreadResolved, model.TierRecord
			if !nt.IsResolved {
				// Somebody re-opened a thread: that is a request.
				kind, tier = model.EvThreadUnresolve, model.TierWake
			}
			d.Events = append(d.Events, model.Event{Kind: kind, Tier: tier, PR: key, ThreadID: id, Path: nt.Path})
		}

		prevComments := map[string]*model.Comment{}
		for _, c := range pt.Comments {
			prevComments[c.ID] = c
		}
		for _, nc := range nt.Comments {
			pc := prevComments[nc.ID]
			if pc == nil {
				ev := model.Event{
					Kind: model.EvCommentNew, Tier: model.TierWake, PR: key, ThreadID: id,
					Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
					Path: nt.Path, URL: nc.URL, Detail: model.FirstLine(nc.Body),
				}
				// Our own words never wake us.
				if strings.EqualFold(nc.Author, me) || nc.IsBot || nt.IsResolved {
					ev.Tier = model.TierRecord
				}
				d.Events = append(d.Events, ev)
				continue
			}
			if pc.Digest != nc.Digest {
				ev := model.Event{
					Kind: model.EvCommentEdited, Tier: model.TierWake, PR: key, ThreadID: id,
					Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
					Path: nt.Path, URL: nc.URL, Detail: model.FirstLine(nc.Body),
				}
				if strings.EqualFold(nc.Author, me) || nc.IsBot || nt.IsResolved {
					ev.Tier = model.TierRecord
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
		d.Events = append(d.Events, model.Event{
			Kind: model.EvThreadGone, Tier: model.TierRecord, PR: key, ThreadID: id, Path: pt.Path,
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
			if pv.BodyDigest() == nr.BodyDigest() {
				continue
			}
			// Same state, different body: some reviewer accounts edit
			// their verdict in place (observed live: an automated
			// "aether-review-bot:v1" identity holding one CHANGES_REQUESTED
			// review across many rounds while swapping the actual blocking
			// finding underneath). The label never changed but the
			// substance did -- that is exactly what a wake exists for.
			tier := model.TierWake
			if nr.IsBot || strings.EqualFold(nr.Author, me) {
				tier = model.TierRecord
			}
			d.Events = append(d.Events, model.Event{
				Kind: model.EvReviewEdited, Tier: tier, PR: key, Author: nr.Author,
				IsBot: nr.IsBot, Stamped: nr.Stamped, Detail: nr.State + " (findings updated)", ReviewID: id,
			})
			continue
		}
		tier := model.TierWake
		if nr.IsBot || strings.EqualFold(nr.Author, me) || nr.State == "COMMENTED" && nr.Body == "" {
			tier = model.TierRecord
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
		d.Events = append(d.Events, model.Event{
			Kind: model.EvReviewSubmitted, Tier: tier, PR: key, Author: nr.Author,
			IsBot: nr.IsBot, Stamped: nr.Stamped, Detail: detail, ReviewID: id,
		})
	}

	// Top-level PR conversation comments: same new/edited shape as thread
	// comments, but with no ThreadID and no resolved-state demotion (a
	// top-level comment cannot be resolved).
	prevPRComments := map[string]*model.Comment{}
	for id, c := range pp.Comments {
		prevPRComments[id] = c
	}
	for _, id := range np.SortedCommentIDs() {
		nc := np.Comments[id]
		pc := prevPRComments[id]
		if pc == nil {
			ev := model.Event{
				Kind: model.EvPRCommentNew, Tier: model.TierWake, PR: key,
				Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
				URL: nc.URL, Detail: model.FirstLine(nc.Body),
			}
			if strings.EqualFold(nc.Author, me) || nc.IsBot || isAutomatonAck(nc.Body) {
				ev.Tier = model.TierRecord
			}
			d.Events = append(d.Events, ev)
			continue
		}
		if pc.Digest != nc.Digest {
			ev := model.Event{
				Kind: model.EvPRCommentEdited, Tier: model.TierWake, PR: key,
				Comment: nc.ID, Author: nc.Author, IsBot: nc.IsBot,
				URL: nc.URL, Detail: model.FirstLine(nc.Body),
			}
			if strings.EqualFold(nc.Author, me) || nc.IsBot || isAutomatonAck(nc.Body) {
				ev.Tier = model.TierRecord
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

// asReviewer re-tiers a delta for a seat reviewing someone else's PR: every
// push and every dismissal of our own review earns a turn, because on repos
// that dismiss stale approvals both mean the approval is gone.
func AsReviewer(d *model.Delta, me string) {
	for i, e := range d.Events {
		ownDismissed := e.Kind == model.EvReviewSubmitted && strings.EqualFold(e.Author, me) &&
			strings.HasSuffix(e.Detail, "DISMISSED")
		if e.Kind == model.EvHeadMoved || ownDismissed {
			d.Events[i].Tier = model.TierWake
		}
	}
}
