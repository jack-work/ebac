package praxis

import (
	"fmt"
	"sort"
	"strings"
)

// Signature is how every comment of ours is signed.
const Signature = "John Kelliher's agents (figaro builder)"

// Title renders "[<topic>][<k>-<n>] <summary>".
func Title(i Issue) string {
	return fmt.Sprintf("[%s][%d-%d] %s", strings.TrimSpace(i.Topic), i.K, i.N, strings.TrimSpace(i.Summary))
}

func sortedChildren(i Issue) []Child {
	cs := append([]Child(nil), i.Children...)
	sort.SliceStable(cs, func(a, b int) bool { return cs[a].K < cs[b].K })
	return cs
}

func childLine(i Issue, c Child) string {
	ref := "(not created yet)"
	if c.Number != 0 {
		ref = fmt.Sprintf("#%d", c.Number)
	}
	st := ""
	if c.Status != "" {
		st = " (" + string(c.Status) + ")"
	}
	sum := ""
	if c.Summary != "" {
		sum = " " + c.Summary
	}
	return fmt.Sprintf("- [%d-%d] %s%s%s", c.K, i.N, ref, sum, st)
}

// Body renders the issue body: a child opens with "Part of #<parent>", "Stack: <k> of <n>",
// "Delivers: ..."; an issue 0 opens with "Delivers: ..." (after "Part of" when it sits under a
// child) and lists its children under "## Stack" in k order. A "## Contract" section follows when
// the form carries one, and the praxis-id marker closes the body when the form has an id.
func Body(i Issue) string {
	var b strings.Builder
	if i.Parent != 0 {
		fmt.Fprintf(&b, "Part of #%d\n", i.Parent)
	}
	if i.K >= 1 {
		fmt.Fprintf(&b, "Stack: %d of %d\n", i.K, i.N)
	}
	fmt.Fprintf(&b, "Delivers: %s\n", strings.TrimSpace(i.Delivers))
	if i.K == 0 {
		b.WriteString("\n## Stack\n")
		cs := sortedChildren(i)
		if len(cs) == 0 {
			fmt.Fprintf(&b, "%s, not split yet.\n", pieces(i.N))
		}
		for _, c := range cs {
			b.WriteString(childLine(i, c) + "\n")
		}
	}
	if c := strings.TrimSpace(i.Contract); c != "" {
		b.WriteString("\n## Contract\n" + c + "\n")
	}
	if i.ID != "" {
		b.WriteString("\n" + Marker(i.ID) + "\n")
	}
	return b.String()
}

// ClaimComment is posted once, when the issue is claimed.
func ClaimComment(i Issue) string {
	who := ""
	if i.Assignee != "" {
		who = fmt.Sprintf(" Assignee: @%s.", i.Assignee)
	}
	return fmt.Sprintf("Claimed by %s.%s", Signature, who)
}

// StatusComment is posted on each status change other than claim, split and done.
func StatusComment(i Issue) string {
	s := fmt.Sprintf("**Status:** %s", i.Status)
	if len(i.PRs) > 0 {
		s += " (" + prList(i.PRs) + ")"
	}
	return s + "\n\n" + Signature
}

// SplitComment is posted when the issue splits into its children; it lists them in k order.
func SplitComment(i Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Status:** split into a stack of %s\n\n", pieces(i.N))
	for _, c := range sortedChildren(i) {
		b.WriteString(childLine(i, c) + "\n")
	}
	b.WriteString("\n" + Signature)
	return b.String()
}

// DoneComment is posted when the issue is done, with its PRs (or, for an issue 0, its children).
func DoneComment(i Issue) string {
	var b strings.Builder
	b.WriteString("**Status:** done")
	if len(i.PRs) > 0 {
		b.WriteString(": fixed by " + prList(i.PRs))
	} else if len(i.Children) > 0 {
		b.WriteString(": every piece of the stack is done")
	}
	b.WriteString("\n\n" + Signature)
	return b.String()
}

func prList(prs []int) string {
	ps := make([]string, len(prs))
	for k, n := range prs {
		ps[k] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(ps, ", ")
}

// Rendered is every text GitHub shows for an issue.
type Rendered struct {
	Title, Body, Comment string
}

// Render validates and renders an issue; Comment is the comment its current status posts.
func Render(i Issue, parent *ParentState) (Rendered, error) {
	if err := Validate(i, parent); err != nil {
		return Rendered{}, err
	}
	r := Rendered{Title: Title(i), Body: Body(i)}
	switch i.Status {
	case Claimed:
		r.Comment = ClaimComment(i)
	case Split:
		r.Comment = SplitComment(i)
	case Done:
		r.Comment = DoneComment(i)
	default:
		r.Comment = StatusComment(i)
	}
	return r, nil
}

func pieces(n int) string {
	if n == 1 {
		return "1 piece"
	}
	return fmt.Sprintf("%d pieces", n)
}
