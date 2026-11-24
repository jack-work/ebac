package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/praxis"
)

// App holds praxis's collaborators; tests replace Forms, Now, NewID and Sleep.
type App struct {
	Forms    Forms
	Register string // the gh-cache register form ref (default @ebac-gh-cache)
	Repo     string // owner/name, e.g. example/repo
	By       string // the requesting seat, recorded on a new-issue request
	Wait     time.Duration
	Poll     time.Duration
	Now      func() time.Time
	NewID    func() string
	Sleep    func(time.Duration)
	Out      io.Writer
}

// Dispatch runs one command line ("issue <verb> ...").
func (a *App) Dispatch(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "issue" {
		return errors.New("usage: praxis issue new|split|status|done|adopt ...")
	}
	verb, rest := args[1], args[2:]
	switch verb {
	case "new":
		return a.cmdNew(ctx, rest)
	case "split":
		return a.cmdSplit(ctx, rest)
	case "status":
		return a.cmdStatus(ctx, rest)
	case "done":
		return a.cmdDone(ctx, rest)
	case "adopt":
		return a.cmdAdopt(ctx, rest)
	}
	return fmt.Errorf("unknown verb %q (new|split|status|done|adopt)", verb)
}

// IssueForm is the child form the sync mints for issue n.
func (a *App) IssueForm(n int) string {
	_, name, _ := strings.Cut(a.Repo, "/")
	return fmt.Sprintf("@ebac-gh-cache-%s-issue-%d", strings.ToLower(name), n)
}

// loadIssue reads issue n's praxis group; ErrNoForm until the sync minted the form.
func (a *App) loadIssue(ctx context.Context, n int) (praxis.Issue, map[string]any, error) {
	m, err := a.Forms.Get(ctx, a.IssueForm(n))
	if err != nil {
		return praxis.Issue{}, nil, err
	}
	if _, ok := lookup(m, "praxis"); !ok {
		return praxis.Issue{Number: n}, m, nil
	}
	is, err := praxis.FromForm(m)
	if err != nil {
		return praxis.Issue{}, nil, fmt.Errorf("issue #%d praxis group: %w", n, err)
	}
	is.Number = n
	return is, m, nil
}

// parentState reads the parent's state for the validator (nil when no parent).
func (a *App) parentState(ctx context.Context, parent int) (*praxis.ParentState, error) {
	if parent == 0 {
		return nil, nil
	}
	p, m, err := a.loadIssue(ctx, parent)
	if err != nil {
		return nil, fmt.Errorf("parent #%d: %w", parent, err)
	}
	open := true
	if st, ok := lookup(m, "gh.state"); ok {
		open = !strings.EqualFold(fmt.Sprint(st), "closed")
	}
	return &praxis.ParentState{Number: parent, Open: open, Status: p.Status}, nil
}

// writePraxis writes one praxis key per field (the keys praxis.FromForm reads).
func (a *App) writePraxis(ctx context.Context, ref string, is praxis.Issue) error {
	for k, v := range praxisKeys(is) {
		if err := a.Forms.Set(ctx, ref, "praxis."+k, v); err != nil {
			return err
		}
	}
	return nil
}

// praxisKeys is the praxis group of an issue, as the form carries it.
func praxisKeys(is praxis.Issue) map[string]any {
	prs := is.PRs
	if prs == nil {
		prs = []int{}
	}
	children := is.Children
	if children == nil {
		children = []praxis.Child{}
	}
	return map[string]any{
		"topic": is.Topic, "k": is.K, "n": is.N, "parent": is.Parent,
		"summary": is.Summary, "delivers": is.Delivers, "contract": is.Contract,
		"status": string(is.Status), "assignee": is.Assignee,
		"prs": prs, "children": children,
	}
}

// ---- new ----

type newFlags struct {
	topic, summary, delivers, contractFile, assignee string
	k, n, parent                                     int
}

func parseNew(args []string) (newFlags, error) {
	var f newFlags
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.topic, "topic", "", "praxis topic, e.g. 'Effort ledger live'")
	fs.IntVar(&f.k, "k", 0, "position in the stack (1..n)")
	fs.IntVar(&f.n, "n", 0, "stack size")
	fs.IntVar(&f.parent, "parent", 0, "parent issue number")
	fs.StringVar(&f.summary, "summary", "", "one-line summary")
	fs.StringVar(&f.delivers, "delivers", "", "one-line Delivers")
	fs.StringVar(&f.contractFile, "contract-file", "", "Contract section body (file)")
	fs.StringVar(&f.assignee, "assignee", "", "GitHub login")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() != 0 {
		return f, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return f, nil
}

func (a *App) cmdNew(ctx context.Context, args []string) error {
	f, err := parseNew(args)
	if err != nil {
		return err
	}
	contract := ""
	if f.contractFile != "" {
		b, err := os.ReadFile(f.contractFile)
		if err != nil {
			return err
		}
		contract = strings.TrimSpace(string(b))
	}
	is := praxis.Issue{Topic: f.topic, K: f.k, N: f.n, Parent: f.parent, Summary: f.summary,
		Delivers: f.delivers, Contract: contract, Assignee: f.assignee, Status: praxis.Claimed}
	n, form, err := a.newIssue(ctx, is)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "#%d %s\n", n, form)
	return nil
}

// newIssue validates, writes the register request, waits for the sync's answer
// and returns the issue number and its form.
func (a *App) newIssue(ctx context.Context, is praxis.Issue) (int, string, error) {
	ps, err := a.parentState(ctx, is.Parent)
	if err != nil {
		return 0, "", err
	}
	if err := praxis.Validate(is, ps); err != nil {
		return 0, "", err
	}
	id := a.NewID()
	is.ID = id
	req := praxisKeys(is)
	req["id"] = id
	delete(req, "prs")
	delete(req, "children")
	req["repo"] = a.Repo
	req["by"] = a.By
	req["at"] = a.Now().UTC().Format(time.RFC3339)
	key := "ours.praxis_new." + id
	if err := a.Forms.Set(ctx, a.Register, key, req); err != nil {
		return 0, "", err
	}
	n, form, err := a.await(ctx, id)
	if err != nil {
		return 0, "", fmt.Errorf("request %s left on %s: %w", key, a.Register, err)
	}
	if err := a.Forms.Delete(ctx, a.Register, key); err != nil {
		return 0, "", err
	}
	return n, form, nil
}

// await polls the register for sync.praxis_new.<id> == created, or its alarm.
func (a *App) await(ctx context.Context, id string) (int, string, error) {
	deadline := a.Now().Add(a.Wait)
	for {
		m, err := a.Forms.Get(ctx, a.Register)
		if err != nil {
			return 0, "", err
		}
		if al, ok := lookup(m, "sync.alarm.praxis."+id); ok {
			b, _ := json.Marshal(al)
			return 0, "", fmt.Errorf("the sync refused the request: %s", b)
		}
		if v, ok := lookup(m, "sync.praxis_new."+id); ok {
			if r, ok := v.(map[string]any); ok && r["state"] == "created" {
				n, err := toInt(r["issue"])
				if err != nil {
					return 0, "", fmt.Errorf("sync.praxis_new.%s.issue: %w", id, err)
				}
				form, _ := r["form"].(string)
				return n, form, nil
			}
		}
		if !a.Now().Before(deadline) {
			return 0, "", fmt.Errorf("no answer from the sync within %s", a.Wait)
		}
		a.Sleep(a.Poll)
	}
}

// ---- split ----

func (a *App) cmdSplit(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: praxis issue split <parent> --child 'summary|delivers' [--child ...]")
	}
	parent, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil {
		return fmt.Errorf("parent %q: %w", args[0], err)
	}
	var kids []string
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("child", "'summary|delivers'", func(s string) error { kids = append(kids, s); return nil })
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(kids) < 2 {
		return errors.New("split needs at least two --child")
	}
	p, _, err := a.loadIssue(ctx, parent)
	if err != nil {
		return fmt.Errorf("parent #%d: %w", parent, err)
	}
	for _, c := range kids {
		if s, d, ok := strings.Cut(c, "|"); !ok || strings.TrimSpace(s) == "" || strings.TrimSpace(d) == "" {
			return fmt.Errorf("child %q: want 'summary|delivers'", c)
		}
	}
	for i, c := range kids {
		s, d, _ := strings.Cut(c, "|")
		child := praxis.Issue{Topic: p.Topic, K: i + 1, N: len(kids), Parent: parent,
			Summary: strings.TrimSpace(s), Delivers: strings.TrimSpace(d), Status: praxis.Claimed}
		n, form, err := a.newIssue(ctx, child)
		if err != nil {
			return fmt.Errorf("child %d/%d: %w", i+1, len(kids), err)
		}
		p.Children = append(p.Children, praxis.Child{Number: n, K: i + 1, Summary: child.Summary, Status: child.Status})
		fmt.Fprintf(a.Out, "#%d %s\n", n, form)
	}
	p.Status = praxis.Split
	return a.writePraxis(ctx, a.IssueForm(parent), p)
}

// ---- status / done ----

func parsePRs(args []string) ([]int, []string, error) {
	var prs []int
	fs := flag.NewFlagSet("prs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("pr", "PR number", func(s string) error {
		n, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
		if err != nil {
			return err
		}
		prs = append(prs, n)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	return prs, fs.Args(), nil
}

func (a *App) setStatus(ctx context.Context, n int, st praxis.Status, prs []int) error {
	is, _, err := a.loadIssue(ctx, n)
	if err != nil {
		return fmt.Errorf("issue #%d: %w", n, err)
	}
	is.Status = st
	for _, p := range prs {
		if !containsInt(is.PRs, p) {
			is.PRs = append(is.PRs, p)
		}
	}
	ps, err := a.parentState(ctx, is.Parent)
	if err != nil {
		return err
	}
	if err := praxis.Validate(is, ps); err != nil {
		return err
	}
	return a.writePraxis(ctx, a.IssueForm(n), is)
}

func (a *App) cmdStatus(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: praxis issue status <n> <status> [--pr N ...]")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil {
		return err
	}
	st, err := parseStatus(args[1])
	if err != nil {
		return err
	}
	prs, rest, err := parsePRs(args[2:])
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("unexpected arguments %q", rest)
	}
	return a.setStatus(ctx, n, st, prs)
}

func (a *App) cmdDone(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: praxis issue done <n> [--pr N ...]")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil {
		return err
	}
	prs, rest, err := parsePRs(args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("unexpected arguments %q", rest)
	}
	return a.setStatus(ctx, n, praxis.Done, prs)
}

// ---- adopt ----

// cmdAdopt writes praxis.* for an existing issue from a JSON spec (the keys
// praxis.FromForm reads). Refuses until the sync has minted the issue's form.
func (a *App) cmdAdopt(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: praxis issue adopt <n> --spec FILE")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil {
		return err
	}
	var spec string
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&spec, "spec", "", "JSON object: the praxis group")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if spec == "" {
		return errors.New("adopt needs --spec FILE")
	}
	b, err := os.ReadFile(spec)
	if err != nil {
		return err
	}
	var g map[string]any
	if err := json.Unmarshal(b, &g); err != nil {
		return fmt.Errorf("%s: %w", spec, err)
	}
	if _, err := a.Forms.Get(ctx, a.IssueForm(n)); err != nil {
		return fmt.Errorf("issue #%d: the sync has not minted its form yet: %w", n, err)
	}
	if _, nested := g["praxis"]; !nested {
		g = map[string]any{"praxis": g}
	}
	is, err := praxis.FromForm(g)
	if err != nil {
		return fmt.Errorf("%s: %w", spec, err)
	}
	is.Number = n
	ps, err := a.parentState(ctx, is.Parent)
	if err != nil {
		return err
	}
	if err := praxis.Validate(is, ps); err != nil {
		return err
	}
	return a.writePraxis(ctx, a.IssueForm(n), is)
}

// ---- helpers ----

func parseStatus(s string) (praxis.Status, error) {
	for _, st := range praxis.Statuses {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("status %q: want one of %v", s, praxis.Statuses)
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func toInt(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case int:
		return t, nil
	case json.Number:
		i, err := t.Int64()
		return int(i), err
	case string:
		return strconv.Atoi(t)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}
