package keel

import (
	"encoding/json"
	"github.com/gluck/ebac/sdk/model"
	"sort"
)

// Reviews on keel: a reviewer pool posts findings (new threads on the doc),
// the author answers (replies, resolves), and each reviewer records a
// verdict on the critic's role form. The critic derives review_state.
//
//	review              {"doc": "<path>", "reviewers": ["reviewer", ...]}   holder-edited
//	verdict.<role>      {"verdict", "rev", "at", "by_aria", "stamp"}   written by `ebac verdict` only (stamped)
//	review_state        written by the critic (below)
//
// review_state is "pass" only when the doc's current revision is known, every
// required reviewer's verdict is pass at it, and no thread on the doc is open; "changes"
// when any required reviewer says changes at the current revision; else
// "waiting", naming what is missing. Required reviewers are review.reviewers,
// which must be named (no reviewers: never pass).

const (
	ReviewKey      = "review"
	VerdictKey     = "verdict"
	ReviewStateKey = "review_state"
)

type (
	ReviewConfig = model.KeelReviewConfig
	Verdict      = model.KeelVerdict
)

var (
	ConfigStampFields = model.KeelConfigStampFields
	StampFields       = model.KeelVerdictStampFields
)

// CheckKey carries, from the core to the plugin, the core's verification
// of the review key and of each named reviewer's verdict (a request key,
// never on the form): the plugin never sees the stamp key.
const CheckKey = model.KeelReviewCheckKey

type ReviewState struct {
	State string `json:"state"`         // pass | changes | waiting
	Doc   string `json:"doc"`           // the reviewed path
	Rev   string `json:"rev,omitempty"` // the doc's current revision ("" = unknown)
	// RevChecked is false when the doc's revision could not be learned: the
	// state is then never pass (fails closed), whatever the verdicts say.
	RevChecked bool     `json:"rev_checked"`
	Open       int      `json:"open"`                 // open threads on the doc
	Missing    []string `json:"missing,omitempty"`    // required reviewers with no verdict
	Stale      []string `json:"stale,omitempty"`      // verdicts at an older revision
	Pending    []string `json:"pending,omitempty"`    // verdict "comment": no decision yet
	ChangesBy  []string `json:"changes_by,omitempty"` // verdict "changes" at the current revision
	Invalid    []string `json:"invalid,omitempty"`    // a verdict value that is none of the three
	// Unstamped verdicts were not written by `ebac verdict` (set by hand, or
	// edited since): they count as missing.
	Unstamped []string `json:"unstamped,omitempty"`
	// ConfigUnstamped: the review key was not written by `ebac review` (or
	// was edited since): never pass.
	ConfigUnstamped bool `json:"config_unstamped,omitempty"`
}

// reviewState derives the state; index is the round's threads index.
func reviewState(cfg ReviewConfig, cfgStamped bool, verdicts map[string]Verdict, index map[string]IndexEntry, rev string, stamped func(role string, v Verdict) bool) ReviewState {
	st := ReviewState{Doc: cfg.Doc, Rev: rev, RevChecked: rev != "", ConfigUnstamped: !cfgStamped}
	for _, e := range index {
		if e.Path == cfg.Doc && !e.Resolved {
			st.Open++
		}
	}
	// The pool is named: never derived from whichever verdicts are present,
	// so a removed or hand-set verdict cannot shrink it.
	required := append([]string(nil), cfg.Reviewers...)
	sort.Strings(required)
	for _, r := range required {
		v, ok := verdicts[r]
		switch {
		case !ok:
			st.Missing = append(st.Missing, r)
		case !stamped(r, v):
			st.Unstamped = append(st.Unstamped, r)
		case st.RevChecked && v.Rev != rev:
			st.Stale = append(st.Stale, r)
		case v.Verdict == "pass":
		case v.Verdict == "changes":
			st.ChangesBy = append(st.ChangesBy, r)
		case v.Verdict == "comment":
			st.Pending = append(st.Pending, r)
		default:
			st.Invalid = append(st.Invalid, r)
		}
	}
	switch {
	case len(st.ChangesBy) > 0:
		st.State = "changes"
	case st.RevChecked && cfgStamped && len(required) > 0 && st.Open == 0 && len(st.Missing)+len(st.Unstamped)+len(st.Stale)+len(st.Pending)+len(st.Invalid) == 0:
		st.State = "pass"
	default:
		st.State = "waiting"
	}
	return st
}

// readReview parses the holder's review key and the reviewers' verdicts.
// ok is false when there is no review configured (no doc).
func readReview(keys map[string]json.RawMessage) (cfg ReviewConfig, verdicts map[string]Verdict, ok bool, err error) {
	raw := keys[ReviewKey]
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, nil, false, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Doc == "" {
		if err == nil {
			err = errNoDoc
		}
		return cfg, nil, false, err
	}
	verdicts = map[string]Verdict{}
	if v := keys[VerdictKey]; len(v) > 0 && string(v) != "null" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return cfg, nil, false, err
		}
		for role, b := range m {
			var vd Verdict
			if json.Unmarshal(b, &vd) != nil {
				vd = Verdict{Verdict: "unreadable"}
			}
			verdicts[role] = vd
		}
	}
	return cfg, verdicts, true, nil
}

type reviewErr string

func (e reviewErr) Error() string { return string(e) }

const errNoDoc = reviewErr(`review: want {"doc": "<path>"}`)
