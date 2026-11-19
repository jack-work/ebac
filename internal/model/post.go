package model

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// The outbox: the review interface, through the form.
//
// A seat (or `ebac post`) writes post.<id> onto the critic's form. Each round
// the poller reads every patch since its watermark, posts what is new to the
// forge, and writes the receipt back onto the same post, in the same patch as
// the new watermark. A post that did not reach an end gets a receipt
// (attempts, error) too, and that receipt is itself a patch past the
// watermark, so the next round sees the post again: retry needs no other
// bookkeeping. If the round's patch is lost, neither moves. A hidden marker in
// every body makes a post that landed but was never acked (a crash, a second
// ebac) recognisable instead of posted twice.

const maxPostAttempts = 5

type Post struct {
	PR      string `json:"pr"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"` // review thread id: reply in that thread
	Path    string `json:"path,omitempty"`     // with Line: a new inline comment
	Line    int    `json:"line,omitempty"`
	Event   string `json:"event,omitempty"` // COMMENT | APPROVE: submit as a review
	// Resolve, with ReplyTo, resolves that thread after the reply (or
	// alone, when Body is empty).
	Resolve bool `json:"resolve,omitempty"`
	// React, with ReplyTo, reacts to the comment that opened that thread.
	React string `json:"react,omitempty"`
	// Rev, keel only, with Path and Line: anchor the new thread on the diff
	// view at this commit instead of the file at main's head.
	Rev string `json:"rev,omitempty"`
	// ByAria and ByRole, keel only: the seat that wrote the post. keel's
	// write door stamps every comment with them ("jack via Figaro <aria8>
	// (<role>): "), so the thread names its writer, never ebac.
	ByAria string `json:"by_aria,omitempty"`
	ByRole string `json:"by_role,omitempty"`
	// ByRoleForm is the role form ByRole resolved to when the post was
	// written (role-checked: ByAria held it); the flush re-checks that the
	// caller still holds that same form.
	ByRoleForm string `json:"by_role_form,omitempty"`

	Sent     *PostReceipt `json:"sent,omitempty"`
	Resolved string       `json:"resolved,omitempty"` // when the thread was resolved
	Reacted  string       `json:"reacted,omitempty"`  // when the reaction landed
	Refused  string       `json:"refused,omitempty"`
	Error    string       `json:"error,omitempty"`
	Attempts int          `json:"attempts,omitempty"`
}

type PostReceipt struct {
	URL string `json:"url"`
	At  string `json:"at"`
	// Recovered: the marker was already on the forge, so nothing was posted.
	Recovered bool `json:"recovered,omitempty"`
}

var (
	keelAria = regexp.MustCompile(`^[0-9a-f]{8,32}$`)
	keelRole = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

// KeelWriter says why a keel post's writer is not one keel's write door
// accepts ("" when it is): by_aria 8-32 hex, by_role 1-40 of [a-z-]. The
// door stamps both onto the comment, so a post never goes out unmarked.
func KeelWriter(aria, role string) string {
	if !keelAria.MatchString(aria) {
		return fmt.Sprintf("by_aria %q: the writing seat's aria id, 8-32 lowercase hex", aria)
	}
	if !keelRole.MatchString(role) {
		return fmt.Sprintf("by_role %q: the writing seat's role, 1-40 of a-z, 0-9 and -", role)
	}
	return ""
}

func (p *Post) HasBody() bool { return strings.TrimSpace(p.Body) != "" }

func (p *Post) Terminal() bool {
	if p.Refused != "" || p.Attempts >= maxPostAttempts {
		return true
	}
	replied := !p.HasBody() || p.Sent != nil
	resolved := !p.Resolve || p.Resolved != ""
	reacted := p.React == "" || p.Reacted != ""
	return replied && resolved && reacted
}

// PostMarker is the hidden dedupe token on every post: a hash of critic and
// post id, so the GHE text names neither the tool nor the critic (Boss order
// 3, 2026-11-16). Posts made under the old <!-- ebac:post:C:I --> marker were
// rewritten in place once (scripts/rewrite-post-markers.py).
func PostMarker(critic, id string) string {
	h := sha256.Sum256([]byte(critic + ":" + id))
	return fmt.Sprintf("<!-- post:%x -->", h[:8])
}

// findMarker returns the URL of a comment of ours already carrying marker.
func FindMarker(pr *PRState, me, marker string) (string, bool) {
	mine := func(author, body string) bool {
		return strings.EqualFold(author, me) && strings.Contains(body, marker)
	}
	for _, t := range pr.Threads {
		for _, c := range t.Comments {
			if c.State == "PENDING" {
				continue // in an unsubmitted review: nobody else can see it
			}
			if mine(c.Author, c.Body) {
				return c.URL, true
			}
		}
	}
	for _, c := range pr.Comments {
		if mine(c.Author, c.Body) {
			return c.URL, true
		}
	}
	for _, r := range pr.Reviews {
		if mine(r.Author, r.Body) {
			return pr.URL, true
		}
	}
	return "", false
}
