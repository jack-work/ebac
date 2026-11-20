package form

import (
	"encoding/json"
	"strings"

	"github.com/gluck/ebac/sdk/model"
)

// The inbox is how comment text reaches a seat now that the form keeps no
// bodies: new and edited comments by others, each cut at
// model.InboxItemBytes (the item names the rest: url, fetch, or the critic's
// inbox_fetch), the newest inboxItems kept and the oldest evicted past
// inboxBytes. It is evicted, not replaced each round, so a seat that sleeps
// through a round still finds the item when it wakes. The whole key reaches
// every new or catching-up studier, so it is kept small (2026-11-17: 24 KB
// on 14 forms, cut to 8 KB).
const (
	inboxItems = 20
	inboxBytes = 8 * 1024
)

// mergeInbox adds fresh items to the current inbox, cuts every item's text
// to model.InboxItemBytes (held items too: a form written under a larger cut
// is cut on its first round), and evicts the oldest until at most inboxItems
// remain within inboxBytes. An evicted item leaves nothing behind.
//
// A Teams item is written once and then immutable (the form-churn order,
// 2026-11-13): a later teams_edited only replaces body and cut and stamps
// edited_at, and only when the body changed; a teams_deleted removes the
// item; a re-sent teams_new for an id already held changes nothing. Every
// unchanged leaf is then left out of the write by the form diff.
func mergeInbox(cur json.RawMessage, fresh map[string]model.InboxItem) map[string]model.InboxItem {
	box := map[string]model.InboxItem{}
	_ = json.Unmarshal(cur, &box)
	for id, it := range box {
		if it.Kind == "" {
			delete(box, id) // null or malformed
		}
	}
	for id, it := range fresh {
		old, held := box[id]
		if !strings.HasPrefix(it.Kind, "teams_") {
			box[id] = it
			continue
		}
		switch {
		case it.Kind == "teams_deleted":
			delete(box, id)
		case !held:
			box[id] = it
		case it.Kind == "teams_edited" && !old.Self && (old.Body != it.Body || old.Cut != it.Cut):
			old.Body, old.Cut, old.EditedAt = it.Body, it.Cut, it.At
			box[id] = old
		}
	}
	for id, it := range box {
		if body, cut := model.CutBody(it.Body, model.InboxItemBytes); cut {
			it.Body, it.Cut = body, true
			box[id] = it
		}
	}
	for len(box) > inboxItems || len(model.MustJSON(box)) > inboxBytes {
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
