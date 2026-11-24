package praxis

import (
	"fmt"
	"strings"
)

// Rule names one way a form breaks the standard; the sync raises sync.alarm.praxis.<rule>.
type Rule string

const (
	RuleBadTopic             Rule = "bad_topic"
	RuleNOutOfRange          Rule = "n_out_of_range"
	RuleKOutOfRange          Rule = "k_out_of_range"
	RuleNoParent             Rule = "no_parent"
	RuleSelfParent           Rule = "self_parent"
	RuleParentClosed         Rule = "parent_closed"
	RuleParentDone           Rule = "parent_done"
	RuleEmptySummary         Rule = "empty_summary"
	RuleEmptyDelivers        Rule = "empty_delivers"
	RuleBadStatus            Rule = "bad_status"
	RuleSplitWithoutChildren Rule = "split_without_children"
	RuleChildOutOfRange      Rule = "child_out_of_range"
	RuleDoneWithoutPRs       Rule = "done_without_prs"
)

// Refusal is the validator's typed error.
type Refusal struct {
	Rule   Rule
	Detail string
}

func (r *Refusal) Error() string { return "praxis: " + string(r.Rule) + ": " + r.Detail }

// AlarmKey is the entity-form key the sync writes for this refusal.
func (r *Refusal) AlarmKey() string { return "sync.alarm.praxis." + string(r.Rule) }

// Validate refuses an issue form that breaks the standard. parent is nil when the issue names no
// parent or the parent's state is unknown to the caller (then only the form itself is checked).
func Validate(i Issue, parent *ParentState) error {
	refuse := func(r Rule, f string, a ...any) error { return &Refusal{Rule: r, Detail: fmt.Sprintf(f, a...)} }
	topic := strings.TrimSpace(i.Topic)
	if topic == "" || strings.ContainsAny(topic, "[]\n") {
		return refuse(RuleBadTopic, "topic %q must be non-empty, one line, without brackets", i.Topic)
	}
	if i.N < 1 {
		return refuse(RuleNOutOfRange, "n = %d, must be at least 1", i.N)
	}
	if i.K < 0 || i.K > i.N {
		return refuse(RuleKOutOfRange, "k = %d, must be 0 (the issue 0 of its stack) up to n = %d", i.K, i.N)
	}
	if i.K >= 1 && i.Parent == 0 {
		return refuse(RuleNoParent, "a child ([%d-%d]) needs its parent issue", i.K, i.N)
	}
	if i.Parent != 0 && i.Parent == i.Number {
		return refuse(RuleSelfParent, "#%d names itself as parent", i.Number)
	}
	if parent != nil && i.Parent != 0 {
		if !parent.Open {
			return refuse(RuleParentClosed, "parent #%d is closed", parent.Number)
		}
		if parent.Status == Done {
			return refuse(RuleParentDone, "parent #%d is done", parent.Number)
		}
	}
	if strings.TrimSpace(i.Summary) == "" {
		return refuse(RuleEmptySummary, "summary is empty")
	}
	if strings.TrimSpace(i.Delivers) == "" {
		return refuse(RuleEmptyDelivers, "delivers is empty")
	}
	if !i.Status.valid() {
		return refuse(RuleBadStatus, "status %q is not one of %v", i.Status, Statuses)
	}
	if i.Status == Split && len(i.Children) == 0 {
		return refuse(RuleSplitWithoutChildren, "status split with no children")
	}
	for _, c := range i.Children {
		if c.K < 1 || c.K > i.N {
			return refuse(RuleChildOutOfRange, "child #%d has k = %d, outside 1..%d", c.Number, c.K, i.N)
		}
	}
	if i.Status == Done && len(i.PRs) == 0 {
		allDone := len(i.Children) > 0
		for _, c := range i.Children {
			if c.Status != Done {
				allDone = false
			}
		}
		if !allDone {
			return refuse(RuleDoneWithoutPRs, "done needs its PRs, or (for an issue 0) every child done")
		}
	}
	return nil
}
