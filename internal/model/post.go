package model

import (
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
