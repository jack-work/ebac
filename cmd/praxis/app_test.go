package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeForms is an in-memory figaro: forms by ref, keys as dotted paths. A
// register request ours.praxis_new.<id> is answered by the fake sync (answer)
// on the next Get of the register.
type fakeForms struct {
	forms   map[string]map[string]any // ref -> dotted key -> JSON-normalised value
	sets    []string                  // "ref key" in order
	deletes []string
	answer  func(id string, req map[string]any) map[string]any // returns keys to add to the register
	next    int
}

func newFake() *fakeForms { return &fakeForms{forms: map[string]map[string]any{}} }

func norm(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeForms) ensure(ref string) map[string]any {
	if f.forms[ref] == nil {
		f.forms[ref] = map[string]any{}
	}
	return f.forms[ref]
}

// nested renders the dotted keys as figaro prints a form (nested objects).
func nested(flat map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range flat {
		parts := strings.Split(k, ".")
		cur := out
		for _, p := range parts[:len(parts)-1] {
			nx, ok := cur[p].(map[string]any)
			if !ok {
				nx = map[string]any{}
				cur[p] = nx
			}
			cur = nx
		}
		cur[parts[len(parts)-1]] = v
	}
	return out
}

func (f *fakeForms) Get(_ context.Context, ref string) (map[string]any, error) {
	m, ok := f.forms[ref]
	if !ok {
		return nil, fmt.Errorf("%s: %w", ref, ErrNoForm)
	}
	if f.answer != nil {
		for k, v := range m {
			if id, ok := strings.CutPrefix(k, "ours.praxis_new."); ok {
				if _, done := m["sync.praxis_new."+id]; done {
					continue
				}
				if _, al := m["sync.alarm.praxis."+id]; al {
					continue
				}
				for ak, av := range f.answer(id, v.(map[string]any)) {
					m[ak] = norm(av)
				}
			}
		}
	}
	return nested(m), nil
}

func (f *fakeForms) Set(_ context.Context, ref, key string, value any) error {
	f.ensure(ref)[key] = norm(value)
	f.sets = append(f.sets, ref+" "+key)
	return nil
}

func (f *fakeForms) Delete(_ context.Context, ref, key string) error {
	delete(f.ensure(ref), key)
	f.deletes = append(f.deletes, ref+" "+key)
	return nil
}

const reg = "@ebac-gh-cache"

func testApp(f *fakeForms) (*App, *bytes.Buffer) {
	var out bytes.Buffer
	ids := []string{"aaaaaaaaaaa1", "aaaaaaaaaaa2", "aaaaaaaaaaa3"}
	i := 0
	clock := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	return &App{
		Forms: f, Register: reg, Repo: "example/repo", By: "@000000c1",
		Wait: 10 * time.Second, Poll: time.Second,
		Now:   func() time.Time { return clock },
		NewID: func() string { id := ids[i]; i++; return id },
		Sleep: func(d time.Duration) { clock = clock.Add(d) },
		Out:   &out,
	}, &out
}

// created answers every request by minting issue 5000+k.
func created(f *fakeForms) func(string, map[string]any) map[string]any {
	return func(id string, req map[string]any) map[string]any {
		f.next++
		n := 5000 + f.next
		form := fmt.Sprintf("@ebac-gh-cache-repo-issue-%d", n)
		f.ensure(form)["gh.state"] = "open"
		return map[string]any{"sync.praxis_new." + id: map[string]any{"state": "created", "issue": n, "form": form}}
	}
}

func seedIssue(f *fakeForms, n int, praxis map[string]any) {
	ref := fmt.Sprintf("@ebac-gh-cache-repo-issue-%d", n)
	m := f.ensure(ref)
	m["gh.state"] = "open"
	for k, v := range praxis {
		m["praxis."+k] = norm(v)
	}
}

func TestNewWritesOneRegisterRequestAndPrintsTheIssue(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	f.answer = created(f)
	app, out := testApp(f)
	err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 0 --n 1 --summary S --delivers D"))
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "#5001 @ebac-gh-cache-repo-issue-5001\n" {
		t.Fatalf("out = %q", got)
	}
	if len(f.sets) != 1 || f.sets[0] != reg+" ours.praxis_new.aaaaaaaaaaa1" {
		t.Fatalf("sets = %v, want exactly the register request", f.sets)
	}
	if len(f.deletes) != 1 || f.deletes[0] != reg+" ours.praxis_new.aaaaaaaaaaa1" {
		t.Fatalf("deletes = %v, want the request removed after created", f.deletes)
	}
	// the request as the sync reads it (captured before the delete through the answer)
}

func TestNewRequestCarriesThePraxisFieldsTheSyncReads(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	var got map[string]any
	f.answer = func(id string, req map[string]any) map[string]any { got = req; return created(f)(id, req) }
	seedIssue(f, 1476, map[string]any{"topic": "Effort", "k": 0, "n": 3, "summary": "P", "delivers": "PD", "status": "claimed"})
	app, _ := testApp(f)
	if err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 2 --n 3 --parent 1476 --summary S --delivers D --assignee ours")); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"topic": "Effort", "k": 2.0, "n": 3.0, "parent": 1476.0, "summary": "S", "delivers": "D",
		"contract": "", "status": "claimed", "assignee": "ours", "id": "aaaaaaaaaaa1",
		"repo": "example/repo", "by": "@000000c1", "at": "2026-11-25T06:00:00Z"}
	if b1, b2 := jsonOf(got), jsonOf(want); b1 != b2 {
		t.Fatalf("request\n got %s\nwant %s", b1, b2)
	}
}

func TestNewRefusedByTheValidatorWritesNothing(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	f.answer = created(f)
	seedIssue(f, 9, map[string]any{"topic": "Effort", "k": 0, "n": 2, "summary": "P", "delivers": "PD", "status": "claimed"})
	app, _ := testApp(f)
	err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 3 --n 2 --parent 9 --summary S --delivers D"))
	if err == nil {
		t.Fatal("k > n accepted")
	}
	if len(f.sets) != 0 {
		t.Fatalf("sets = %v, want none", f.sets)
	}
}

func TestNewLeavesTheRequestWhenTheSyncRefuses(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	f.answer = func(id string, _ map[string]any) map[string]any {
		return map[string]any{"sync.alarm.praxis." + id: map[string]any{"rule": "x", "detail": "no"}}
	}
	app, _ := testApp(f)
	err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 0 --n 1 --summary S --delivers D"))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v, want the sync's refusal", err)
	}
	if len(f.deletes) != 0 {
		t.Fatalf("deletes = %v, want the request kept for the maintainer", f.deletes)
	}
}

func TestNewTimesOutWithoutAnAnswer(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	app, _ := testApp(f)
	err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 0 --n 1 --summary S --delivers D"))
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestNewChildOfAClosedParentIsRefused(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	f.answer = created(f)
	seedIssue(f, 1476, map[string]any{"topic": "Effort ledger live", "k": 0, "n": 1, "summary": "P", "delivers": "D", "status": "done"})
	f.forms["@ebac-gh-cache-repo-issue-1476"]["gh.state"] = "closed"
	app, _ := testApp(f)
	err := app.Dispatch(context.Background(), strings.Fields(
		"issue new --topic Effort --k 1 --n 1 --parent 1476 --summary S --delivers D"))
	if err == nil {
		t.Fatal("a child of a closed parent was accepted")
	}
	if len(f.sets) != 0 {
		t.Fatalf("sets = %v, want none", f.sets)
	}
}

func TestSplitCreatesTheChildrenThenMarksTheParentSplit(t *testing.T) {
	f := newFake()
	f.ensure(reg)
	f.answer = created(f)
	seedIssue(f, 1476, map[string]any{"topic": "Effort ledger live", "k": 0, "n": 1, "summary": "P", "delivers": "PD", "status": "claimed"})
	app, out := testApp(f)
	err := app.Dispatch(context.Background(), []string{"issue", "split", "1476",
		"--child", "first|does one", "--child", "second|does two"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("out = %q", out.String())
	}
	p := f.forms["@ebac-gh-cache-repo-issue-1476"]
	if p["praxis.status"] != "split" {
		t.Fatalf("parent status = %v", p["praxis.status"])
	}
	if got := jsonOf(p["praxis.children"]); got != `[{"k":1,"number":5001,"status":"claimed","summary":"first"},{"k":2,"number":5002,"status":"claimed","summary":"second"}]` {
		t.Fatalf("children = %s", got)
	}
}

func TestStatusAddsAPROnceAndValidates(t *testing.T) {
	f := newFake()
	seedIssue(f, 1476, map[string]any{"topic": "Effort ledger live", "k": 0, "n": 1, "summary": "P", "delivers": "PD", "status": "split", "children": []int{1300}})
	seedIssue(f, 1300, map[string]any{"topic": "Effort ledger live", "k": 1, "n": 1, "parent": 1476, "summary": "S", "delivers": "D", "status": "claimed", "prs": []int{1500}})
	app, _ := testApp(f)
	ctx := context.Background()
	if err := app.Dispatch(ctx, strings.Fields("issue status 1300 pr-open --pr 1500 --pr 1501")); err != nil {
		t.Fatal(err)
	}
	m := f.forms["@ebac-gh-cache-repo-issue-1300"]
	if m["praxis.status"] != "pr-open" || jsonOf(m["praxis.prs"]) != "[1500,1501]" {
		t.Fatalf("status=%v prs=%s", m["praxis.status"], jsonOf(m["praxis.prs"]))
	}
	if err := app.Dispatch(ctx, strings.Fields("issue status 1300 bogus")); err == nil {
		t.Fatal("unknown status accepted")
	}
	if err := app.Dispatch(ctx, strings.Fields("issue done 1300")); err != nil {
		t.Fatal(err)
	}
	if m["praxis.status"] != "done" {
		t.Fatalf("status=%v", m["praxis.status"])
	}
}

func TestAdoptRefusesUntilTheSyncMintedTheForm(t *testing.T) {
	f := newFake()
	app, _ := testApp(f)
	spec := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(spec, []byte(`{"topic":"Effort ledger live","k":0,"n":1,"summary":"S","delivers":"D","status":"claimed"}`), 0o644)
	ctx := context.Background()
	if err := app.Dispatch(ctx, []string{"issue", "adopt", "1300", "--spec", spec}); err == nil || !strings.Contains(err.Error(), "not minted") {
		t.Fatalf("err = %v, want refused before the form exists", err)
	}
	if len(f.sets) != 0 {
		t.Fatalf("sets = %v", f.sets)
	}
	f.ensure("@ebac-gh-cache-repo-issue-1300")["gh.state"] = "open"
	if err := app.Dispatch(ctx, []string{"issue", "adopt", "1300", "--spec", spec}); err != nil {
		t.Fatal(err)
	}
	if f.forms["@ebac-gh-cache-repo-issue-1300"]["praxis.summary"] != "S" {
		t.Fatalf("form = %v", f.forms["@ebac-gh-cache-repo-issue-1300"])
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(norm(v)); return string(b) }
