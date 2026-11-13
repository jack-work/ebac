package form

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync/atomic"
)

// itemEditable are the only fields of an EXISTING inbox item a write may
// change: a Teams edit (body, its cut flag, edited_at). Everything else on an
// item is written once, at delivery: re-touching an old item's at, url or
// kind re-sent it to every studier (context audit 2026-11-13: 21% of study
// bytes were old entries re-touched).
var itemEditable = map[string]bool{"body": true, "cut": true, "edited_at": true}

// guardedItemKeys are the top-level keys whose members are immutable items.
var guardedItemKeys = []string{"inbox"}

// GuardRefused counts, for this process, the item fields a write tried to
// change and the guard kept as they were.
var GuardRefused atomic.Int64

// guardItems keeps every non-editable field of an item that is already on
// the form exactly as the form has it, in the keys a write sets. A new item
// and a removed item pass untouched. It returns the fields it kept and their
// paths, for the log.
func guardItems(cur, set map[string]json.RawMessage) (int, []string) {
	n := 0
	var paths []string
	for _, key := range guardedItemKeys {
		next, ok := set[key]
		if !ok || len(cur[key]) == 0 {
			continue
		}
		var was, now map[string]map[string]json.RawMessage
		if json.Unmarshal(cur[key], &was) != nil || json.Unmarshal(next, &now) != nil {
			continue // not a map of items: nothing this guard can reason about
		}
		touched := false
		for id, item := range now {
			old, held := was[id]
			if !held || item == nil || old == nil {
				continue
			}
			fields := map[string]bool{}
			for f := range old {
				fields[f] = true
			}
			for f := range item {
				fields[f] = true
			}
			for f := range fields {
				if itemEditable[f] || bytes.Equal(old[f], item[f]) {
					continue
				}
				if v, ok := old[f]; ok {
					item[f] = v
				} else {
					delete(item, f)
				}
				n++
				touched = true
				paths = append(paths, key+"."+id+"."+f)
			}
		}
		if touched {
			b, err := json.Marshal(now)
			if err == nil {
				set[key] = b
			}
		}
	}
	sort.Strings(paths)
	GuardRefused.Add(int64(n))
	return n, paths
}
