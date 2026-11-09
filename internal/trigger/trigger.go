// Package trigger is the core's one decision about every event, from every
// source: does it wake the seat, is it only recorded, or is it dropped.
//
// Sources only translate an upstream into model.Events and give each one the
// tier their own knowledge suggests (a seat's own post, a resolved thread, a
// bot stamp). The rules then decide, the same way for every source. They live
// on the critic's ROLE FORM under key `trigger`, so whoever holds the role can
// change them with `figaro set`, and ebac reads them every round.
//
// The value is an ordered JSON list; the first rule that matches wins:
//
//	[{"contains": ["Agent-Authored on behalf of", "Sent by Figaro"], "action": "wake"},
//	 {"from": "operator", "action": "record"},
//	 {"default": true, "action": "wake"}]
//
// Match fields (all given fields must match; a list matches if any entry does):
//
//	from      the sender's display name, sender id or bare guid, case-insensitive;
//	          the word "operator" is the Boss (OperatorSender, the Teams id).
//	not_from  matches when the sender is NONE of these.
//	contains  case-insensitive substrings of the full message text.
//	regex     a Go regular expression over the full text (add (?i) for case).
//	kind      the event kind, e.g. teams_new, thread_new, comment_new, author_new.
//
// Actions: wake (worth a turn), record (inbox only, never wakes), drop
// (neither inbox nor wake), keep (leave the tier the source chose).
//
// The last rule must be {"default": true, "action": ...} and carry no match
// fields. A rule set that does not parse is never applied: ebac keeps the last
// good set, writes the error to the form key trigger_error, and the round
// continues.
package trigger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// OperatorSender is the Boss's Teams sender id; `from: operator` matches it,
// the bare guid, or either in any case.
const OperatorSender = "8:orgid:bc3cb0f4-1239-4ec7-a528-4913522f79c5"

// FormKey is the role-form key the rules live in; ErrorKey says why the
// current value is not in force.
const (
	FormKey  = "trigger"
	ErrorKey = "trigger_error"
)

type Action string

const (
	Wake   Action = "wake"
	Record Action = "record"
	Drop   Action = "drop"
	Keep   Action = "keep"
)

// Disclosures are the footers figaro-sent Teams messages carry. Those messages
// go out under the Boss's account, so they must be matched BEFORE
// `from: operator`, or every agent alert in a Boss DM would be muted.
var Disclosures = []string{"Agent-Authored on behalf of", "Sent by Figaro"}

// list is a JSON string or a list of strings.
type list []string

func (l *list) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = list{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("want a string or a list of strings, got %s", b)
	}
	*l = many
	return nil
}

type Rule struct {
	From     list   `json:"from,omitempty"`
	NotFrom  list   `json:"not_from,omitempty"`
	Contains list   `json:"contains,omitempty"`
	Regex    string `json:"regex,omitempty"`
	Kind     list   `json:"kind,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Action   Action `json:"action"`

	re *regexp.Regexp
}

type Rules []Rule

func (r Rule) hasMatch() bool {
	return len(r.From) > 0 || len(r.NotFrom) > 0 || len(r.Contains) > 0 || r.Regex != "" || len(r.Kind) > 0
}

// Parse validates a rule set. A value stored as a JSON string holding the list
// (a hand-typed `figaro set`) is accepted too.
func Parse(raw json.RawMessage) (Rules, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("empty rule set")
	}
	if raw[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, err
		}
		raw = json.RawMessage(inner)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rs Rules
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("not a rule list: %w", err)
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("empty rule set: a final {\"default\": true, \"action\": ...} is required")
	}
	for i := range rs {
		r := &rs[i]
		at := fmt.Sprintf("rule %d", i+1)
		switch r.Action {
		case Wake, Record, Drop, Keep:
		case "":
			return nil, fmt.Errorf("%s: action is required (wake|record|drop|keep)", at)
		default:
			return nil, fmt.Errorf("%s: unknown action %q (wake|record|drop|keep)", at, r.Action)
		}
		if r.Default {
			if r.hasMatch() {
				return nil, fmt.Errorf("%s: a default rule takes no match fields", at)
			}
			if i != len(rs)-1 {
				return nil, fmt.Errorf("%s: the default rule must be last (rules after it never run)", at)
			}
			continue
		}
		if !r.hasMatch() {
			return nil, fmt.Errorf("%s: no match fields; it would match everything: mark it {\"default\": true}", at)
		}
		if r.Regex != "" {
			re, err := regexp.Compile(r.Regex)
			if err != nil {
				return nil, fmt.Errorf("%s: regex: %w", at, err)
			}
			r.re = re
		}
	}
	if !rs[len(rs)-1].Default {
		return nil, fmt.Errorf("the last rule must be {\"default\": true, \"action\": ...}")
	}
	return rs, nil
}

func (r Rule) matches(e model.Event) bool {
	if len(r.From) > 0 && !anySender(r.From, e) {
		return false
	}
	if len(r.NotFrom) > 0 && anySender(r.NotFrom, e) {
		return false
	}
	text := e.Text
	if text == "" {
		text = e.Detail
	}
	if len(r.Contains) > 0 {
		low, hit := strings.ToLower(text), false
		for _, c := range r.Contains {
			if c != "" && strings.Contains(low, strings.ToLower(c)) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if r.re != nil && !r.re.MatchString(text) {
		return false
	}
	if len(r.Kind) > 0 {
		hit := false
		for _, k := range r.Kind {
			if strings.EqualFold(k, string(e.Kind)) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func bareGUID(id string) string { return strings.TrimPrefix(strings.ToLower(id), "8:orgid:") }

func anySender(names list, e model.Event) bool {
	for _, n := range names {
		if senderIs(n, e) {
			return true
		}
	}
	return false
}

func senderIs(name string, e model.Event) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.EqualFold(name, "operator") {
		name = OperatorSender
	}
	id := strings.ToLower(e.AuthorID)
	low := strings.ToLower(name)
	if strings.EqualFold(e.Author, name) {
		return true
	}
	return id != "" && (low == id || bareGUID(low) == bareGUID(id))
}

// Apply runs the rules over events: the first matching rule decides each one.
// It returns the events that survive (with their tier set) and those dropped.
func (rs Rules) Apply(events []model.Event) (kept, dropped []model.Event) {
	for _, e := range events {
		act := Keep
		for _, r := range rs {
			if r.Default || r.matches(e) {
				act = r.Action
				break
			}
		}
		switch act {
		case Wake:
			e.Tier = model.TierWake
		case Record:
			e.Tier = model.TierRecord
		case Drop:
			dropped = append(dropped, e)
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

// Teams is the default set for a Teams chat: a deletion is recorded; a
// figaro-sent message (it carries a disclosure footer, under the Boss's
// account) wakes; the Boss himself is recorded; everyone else wakes.
func Teams() Rules {
	return Rules{
		{Kind: list{"teams_deleted"}, Action: Record},
		{Contains: list(Disclosures), Action: Wake},
		{From: list{"operator"}, Action: Record},
		{Default: true, Action: Wake},
	}
}

// MigrateQuiet is the Teams default set with a pre-trigger quiet list folded
// in: every quiet sender but the operator (by the word, id, guid or the given
// display name) recorded just before the final default.
func MigrateQuiet(quiet []string, operatorName string) Rules {
	op := model.Event{Author: operatorName, AuthorID: OperatorSender}
	var keep list
	for _, q := range quiet {
		if q = strings.TrimSpace(q); q != "" && !strings.EqualFold(q, "operator") && !senderIs(q, op) {
			keep = append(keep, q)
		}
	}
	rs := Teams()
	if len(keep) == 0 {
		return rs
	}
	last := rs[len(rs)-1]
	return append(rs[:len(rs)-1], Rule{From: keep, Action: Record}, last)
}

// SourceDefault leaves every event at the tier its source chose.
func SourceDefault() Rules { return Rules{{Default: true, Action: Keep}} }

// DefaultFor is the set `ebac add-*` writes, and the set in force on a role
// form with no trigger key.
func DefaultFor(source string) Rules {
	if source == "teams-chat" {
		return Teams()
	}
	if source == "github-releases" {
		return Rules{{Default: true, Action: Wake}} // every release wakes (W7 item 3)
	}
	if source == "band-hub" {
		return Rules{{Default: true, Action: Drop}} // a mirror: studiers see the form, nobody is woken
	}
	return SourceDefault()
}

// JSON is the rule set as it is written to the form.
func (rs Rules) JSON() json.RawMessage {
	b, _ := json.Marshal(rs)
	return b
}
