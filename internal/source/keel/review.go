package keel

import (
	"encoding/json"
	"sort"
)

// Reviews on keel: a reviewer pool posts findings (new threads on the doc),
// the author answers (replies, resolves), and each reviewer records a
// verdict on the critic's role form. The critic derives review_state.
//
//	review              {"doc": "<path>", "reviewers": ["reviewer", ...]}   holder-edited
//	verdict.<role>      {"verdict": "pass"|"changes"|"comment", "rev": "<sha>", "at": "<RFC3339>"}   written by each reviewer
//	review_state        written by the critic (below)
//
// review_state is "pass" only when every required reviewer's verdict is pass
// at the doc's current revision and no thread on the doc is open; "changes"
// when any required reviewer says changes at the current revision; else
// "waiting", naming what is missing. Required reviewers are review.reviewers,
// else every role with a verdict.

const (
	ReviewKey      = "review"
	VerdictKey     = "verdict"
	ReviewStateKey = "review_state"
)

type ReviewConfig struct {
	Doc       string   `json:"doc"`
	Reviewers []string `json:"reviewers,omitempty"`
}

type Verdict struct {
	Verdict string `json:"verdict"`
	Rev     string `json:"rev,omitempty"`
	At      string `json:"at,omitempty"`
}

type ReviewState struct {
	State string `json:"state"`         // pass | changes | waiting
	Doc   string `json:"doc"`           // the reviewed path
	Rev   string `json:"rev,omitempty"` // the doc's current revision ("" = unknown)
	// RevChecked is false when the doc's revision could not be learned:
	// verdicts are then taken at face value.
	RevChecked bool     `json:"rev_checked"`
	Open       int      `json:"open"`                 // open threads on the doc
	Missing    []string `json:"missing,omitempty"`    // required reviewers with no verdict
	Stale      []string `json:"stale,omitempty"`      // verdicts at an older revision
	Pending    []string `json:"pending,omitempty"`    // verdict "comment": no decision yet
	ChangesBy  []string `json:"changes_by,omitempty"` // verdict "changes" at the current revision
	Invalid    []string `json:"invalid,omitempty"`    // a verdict value that is none of the three
}

// reviewState derives the state; index is the round's threads index.
func reviewState(cfg ReviewConfig, verdicts map[string]Verdict, index map[string]IndexEntry, rev string) ReviewState {
	st := ReviewState{Doc: cfg.Doc, Rev: rev, RevChecked: rev != ""}
	for _, e := range index {
		if e.Path == cfg.Doc && !e.Resolved {
			st.Open++
		}
	}
	required := cfg.Reviewers
	if len(required) == 0 {
		for r := range verdicts {
			required = append(required, r)
		}
	}
	required = append([]string(nil), required...)
	sort.Strings(required)
	for _, r := range required {
		v, ok := verdicts[r]
		switch {
		case !ok:
			st.Missing = append(st.Missing, r)
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
	case len(required) > 0 && st.Open == 0 && len(st.Missing)+len(st.Stale)+len(st.Pending)+len(st.Invalid) == 0:
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
