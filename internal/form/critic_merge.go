package form

import (
	"bytes"
	"encoding/json"
	"sort"
)

// mergeCritic is SaveRound's three-way merge of the critic key, field by
// top-level field: base is the critic as this process loaded it, mine the
// critic it is saving, theirs the critic on the form now.
//
// Why a merge and not a version check: the role form's version moves on every
// unrelated write (inbox, posts, the seat's ack, trigger, studies), so
// refusing a save on a newer version would refuse most rounds and lose their
// runtime. Only the critic key is at stake, and only a field both sides
// changed is a real conflict.
//
//   - a field the round did not change takes the form's value (a live
//     `figaro set critic.<field>` survives);
//   - a field only the round changed takes the round's value;
//   - a field both changed to different values keeps the FORM's value and is
//     reported: a source recomputes its own fields every round, so a lost
//     round-side change comes back next round, while a person's set would be
//     lost for good.
//
// With no base (a critic this process created) or nothing on the form, mine
// is written whole, as before.
func mergeCritic(base, mine, theirs json.RawMessage) (json.RawMessage, []string, error) {
	if len(base) == 0 || len(theirs) == 0 {
		return mine, nil, nil
	}
	var b, m, t map[string]json.RawMessage
	for _, x := range []struct {
		raw json.RawMessage
		to  *map[string]json.RawMessage
	}{{base, &b}, {mine, &m}, {theirs, &t}} {
		if err := json.Unmarshal(x.raw, x.to); err != nil {
			return nil, nil, err
		}
	}
	keys := map[string]bool{}
	for _, mp := range []map[string]json.RawMessage{b, m, t} {
		for k := range mp {
			keys[k] = true
		}
	}
	out := map[string]json.RawMessage{}
	var conflicts []string
	for k := range keys {
		bv, mv, tv := canon(b[k]), canon(m[k]), canon(t[k])
		var pick json.RawMessage
		switch {
		case bytes.Equal(mv, tv), bytes.Equal(tv, bv):
			pick = m[k]
		case bytes.Equal(mv, bv):
			pick = t[k]
		default:
			pick = t[k]
			conflicts = append(conflicts, k)
		}
		if pick != nil {
			out[k] = pick
		}
	}
	sort.Strings(conflicts)
	raw, err := json.Marshal(out)
	return raw, conflicts, err
}

// canon is a JSON value in one layout (sorted keys, no spaces), so equal
// values compare equal however the daemon stored them; absent is nil.
func canon(v json.RawMessage) []byte {
	if v == nil {
		return nil
	}
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return v
	}
	b, _ := json.Marshal(x)
	return b
}
