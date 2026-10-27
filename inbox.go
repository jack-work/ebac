package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// The inbox is how comment text reaches a seat now that the form keeps no
// bodies: new and edited comments by others, each cut at inboxItemBytes,
// with the oldest evicted once the inbox passes inboxBytes. It is evicted by
// size, not replaced each round, so a seat that sleeps through a round still
// finds the item when it wakes.
const (
	inboxItemBytes = 2000
	inboxBytes     = 24 * 1024
)

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
	Body     string `json:"body"`
	Cut      bool   `json:"cut,omitempty"` // the full text is at URL
}

// inboxItems turns a round's events into inbox entries, keyed by comment or
// review id. The operator's own posts are skipped: the seat wrote them.
func inboxItems(next *model.Snapshot, events []model.Event, me string, now time.Time) map[string]InboxItem {
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
		it.Body, it.Cut = model.CutBody(body, inboxItemBytes)
		out[id] = it
	}
	for _, e := range events {
		pr := next.PRs[e.PR]
		if pr == nil {
			continue
		}
		switch e.Kind {
		case model.EvThreadNew:
			if th := pr.Threads[e.ThreadID]; th != nil {
				for _, c := range th.Comments {
					add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL, Thread: e.ThreadID}, c.Body)
				}
			}
		case model.EvCommentNew, model.EvCommentEdited:
			if th := pr.Threads[e.ThreadID]; th != nil {
				for _, c := range th.Comments {
					if c.ID == e.Comment {
						add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL, Thread: e.ThreadID}, c.Body)
					}
				}
			}
		case model.EvPRCommentNew, model.EvPRCommentEdited:
			if c := pr.Comments[e.Comment]; c != nil {
				add(c.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: c.Author, URL: c.URL}, c.Body)
			}
		case model.EvReviewSubmitted, model.EvReviewEdited:
			if r := pr.Reviews[e.ReviewID]; r != nil && strings.TrimSpace(r.Body) != "" {
				add(r.ID, InboxItem{PR: string(e.PR), Kind: string(e.Kind), By: r.Author, URL: pr.URL}, r.Body)
			}
		}
	}
	return out
}

// mergeInbox adds fresh items to the current inbox and evicts the oldest
// until it fits inboxBytes.
func mergeInbox(cur json.RawMessage, fresh map[string]InboxItem) map[string]InboxItem {
	box := map[string]InboxItem{}
	_ = json.Unmarshal(cur, &box)
	for id, it := range box {
		if it.Kind == "" {
			delete(box, id) // null or malformed
		}
	}
	for id, it := range fresh {
		box[id] = it
	}
	for len(model.MustJSON(box)) > inboxBytes {
		oldest := ""
		for id, it := range box {
			if oldest == "" || it.At < box[oldest].At || (it.At == box[oldest].At && id < oldest) {
				oldest = id
			}
		}
		delete(box, oldest)
	}
	return box
}
