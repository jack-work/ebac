package model

import (
	"strings"
	"time"
)

// InboxItem is one comment or message as a seat reads it on the form inbox.
type InboxItem struct {
	PR       string `json:"pr,omitempty"`
	Chat     string `json:"chat,omitempty"`
	FromID   string `json:"from_id,omitempty"` // teams sender id (8:orgid:...)
	Time     string `json:"time,omitempty"`    // when the message was written (teams)
	HasImage bool   `json:"has_image,omitempty"`
	Fetch    string `json:"fetch,omitempty"` // exact command for the full text
	Kind     string `json:"kind"`
	By       string `json:"by"`
	At       string `json:"at"`
	URL      string `json:"url,omitempty"`
	Thread   string `json:"thread,omitempty"`
	Path     string `json:"path,omitempty"`  // keel: the file the thread is anchored to
	Line     int    `json:"line,omitempty"`  // keel: its first line
	Quote    string `json:"quote,omitempty"` // keel: the selected text, cut
	Body     string `json:"body"`
	Cut      bool   `json:"cut,omitempty"` // the full text is at URL
	// EditedAt is when an edit of a Teams message was seen; the item keeps
	// its first At, Kind and every other field (an item is written once).
	EditedAt string `json:"edited_at,omitempty"`
	// Self marks a Teams message by the critic's own account (chat.self):
	// written once, compact (first line), never updated; the full text is
	// at the critic's inbox_fetch with this item's message id.
	Self bool `json:"self,omitempty"`
}

// inboxItems turns a round's events into inbox entries, keyed by comment or
// review id. The operator's own posts are skipped: the seat wrote them.
func InboxItems(next *Snapshot, events []Event, me string, now time.Time) map[string]InboxItem {
	out := map[string]InboxItem{}
	if next == nil {
		return out
	}
	at := now.UTC().Format(time.RFC3339)
	add := func(id string, it InboxItem, body string) {
		if id == "" || strings.EqualFold(it.By, me) {
			return
		}
		it.At = at
		it.Body, it.Cut = CutBody(body, InboxItemBytes)
		out[id] = it
	}
	for _, e := range events {
		pr := next.PRs[e.PR]
		if pr == nil {
			continue
		}
		switch e.Kind {
		case EvThreadNew:
			if th := pr.Threads[e.ThreadID]; th != nil {
				for _, c := range th.Comments {
					add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL, Thread: e.ThreadID}, c.Body)
				}
			}
		case EvCommentNew, EvCommentEdited:
			if th := pr.Threads[e.ThreadID]; th != nil {
				for _, c := range th.Comments {
					if c.ID == e.Comment {
						add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL, Thread: e.ThreadID}, c.Body)
					}
				}
			}
		case EvPRCommentNew, EvPRCommentEdited:
			if c := pr.Comments[e.Comment]; c != nil {
				add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL}, c.Body)
			}
		case EvReviewSubmitted, EvReviewEdited:
			if r := pr.Reviews[e.ReviewID]; r != nil && strings.TrimSpace(r.Body) != "" {
				add(r.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: r.Author, URL: pr.URL}, r.Body)
			}
		}
	}
	return out
}
