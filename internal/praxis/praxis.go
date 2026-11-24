// Package praxis is the praxis issue standard as code: the schema of a praxis issue's entity form
// (praxis.topic, k, n, parent, summary, delivers, status, assignee, prs[], children[]), the
// validator that refuses a form breaking the standard, and the renderer of every text GitHub shows
// for it (title, body, claim, status, split and done comments). The gh-cache sync renders from
// here and turns a refusal into sync.alarm.praxis.<rule>; the standard's doc is generated from
// here (GenDoc), so the doc cannot drift from what is posted.
package praxis

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Status is an issue's praxis status.
type Status string

const (
	Claimed    Status = "claimed"
	Split      Status = "split"
	InProgress Status = "in-progress"
	PROpen     Status = "pr-open"
	Done       Status = "done"
)

// Statuses lists every status in order of a normal run.
var Statuses = []Status{Claimed, Split, InProgress, PROpen, Done}

func (s Status) valid() bool {
	for _, v := range Statuses {
		if s == v {
			return true
		}
	}
	return false
}

// Child is one entry of a parent's child list.
type Child struct {
	Number  int    `json:"number"`
	K       int    `json:"k"`
	Summary string `json:"summary"`
	Status  Status `json:"status"`
}

// Issue is a praxis issue's entity form (the praxis.* keys).
type Issue struct {
	Number   int     `json:"number,omitempty"` // 0 until GitHub created it
	Topic    string  `json:"topic"`
	K        int     `json:"k"`
	N        int     `json:"n"`
	Parent   int     `json:"parent,omitempty"`
	Summary  string  `json:"summary"`
	Delivers string  `json:"delivers"`
	Status   Status  `json:"status"`
	Assignee string  `json:"assignee,omitempty"`
	PRs      []int   `json:"prs,omitempty"`
	Children []Child `json:"children,omitempty"`
	// Contract is the optional body of the issue's "## Contract" section (issue template keys,
	// one per line), rendered verbatim under the heading.
	Contract string `json:"contract,omitempty"`
	// ID is the new-issue id (12 hex); the body carries it as a hidden marker so a retried
	// create adopts the issue it already made.
	ID string `json:"id,omitempty"`
}

// Marker renders the hidden praxis-id marker the body ends with.
func Marker(id string) string { return "<!-- praxis-id: " + id + " -->" }

// ParentState is what the validator needs to know about an issue's parent.
type ParentState struct {
	Number int
	Open   bool
	Status Status
}

// FromForm reads the praxis keys of an entity form: either a nested {"praxis": {...}} object (as
// `figaro form -j` prints dotted keys) or flat "praxis.<key>" entries.
func FromForm(form map[string]any) (Issue, error) {
	src := map[string]any{}
	if p, ok := form["praxis"].(map[string]any); ok {
		src = p
	}
	for k, v := range form {
		if strings.HasPrefix(k, "praxis.") {
			src[strings.TrimPrefix(k, "praxis.")] = v
		}
	}
	if ch, ok := src["children"].([]any); ok {
		norm := make([]any, 0, len(ch))
		for _, c := range ch {
			switch v := c.(type) {
			case float64:
				norm = append(norm, map[string]any{"number": v})
			case string:
				n, _ := strconv.Atoi(strings.TrimPrefix(v, "#"))
				norm = append(norm, map[string]any{"number": float64(n)})
			default:
				norm = append(norm, v)
			}
		}
		src["children"] = norm
	}
	for _, k := range []string{"k", "n", "parent", "number"} {
		if v, ok := src[k].(string); ok {
			n, err := strconv.Atoi(strings.TrimPrefix(v, "#"))
			if err != nil {
				return Issue{}, fmt.Errorf("praxis form: %s: %q is not a number", k, v)
			}
			src[k] = float64(n)
		}
	}
	b, err := json.Marshal(src)
	if err != nil {
		return Issue{}, err
	}
	var i Issue
	if err := json.Unmarshal(b, &i); err != nil {
		return Issue{}, fmt.Errorf("praxis form: %w", err)
	}
	if n, ok := form["number"]; ok && i.Number == 0 {
		switch v := n.(type) {
		case float64:
			i.Number = int(v)
		case string:
			i.Number, _ = strconv.Atoi(v)
		}
	}
	return i, nil
}
