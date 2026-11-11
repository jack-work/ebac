package fleet

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// fill sets every string in v (recursively; one element in each slice, map and pointer) to s.
func fill(v reflect.Value, s string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Ptr:
		if v.Type().Elem().Kind() == reflect.Struct {
			v.Set(reflect.New(v.Type().Elem()))
			fill(v.Elem(), s)
		}
	case reflect.Struct:
		if v.Type().PkgPath() == "time" {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), s)
			}
		}
	case reflect.Slice:
		if k := v.Type().Elem().Kind(); k == reflect.Struct || k == reflect.String {
			e := reflect.New(v.Type().Elem()).Elem()
			fill(e, s)
			v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
		}
	}
}

func stringPaths(node any, at string, out map[string]bool) {
	switch x := node.(type) {
	case map[string]any:
		for k, v := range x {
			p := k
			if at != "" {
				p = at + "." + k
			}
			stringPaths(v, p, out)
		}
	case []any:
		for _, v := range x {
			stringPaths(v, at+"[]", out)
		}
	case string:
		out[at] = true
	}
}

// Every configuration field that can hold a form address (by its name: role, alert, reviewer, warn, form, owner,
// target, to) is either a role path Repoint rewrites or listed as not a role. A new one fails this test until it is
// classified.
func TestRolePathsCoverTheModel(t *testing.T) {
	var w model.Critic
	fill(reflect.ValueOf(&w).Elem(), "@ab")
	raw, _ := json.Marshal(w)
	var root any
	_ = json.Unmarshal(raw, &root)
	paths := map[string]bool{}
	stringPaths(root, "", paths)
	if len(paths) < 40 {
		t.Fatalf("filled only %d string fields: the walk is blind", len(paths))
	}
	known := map[string]bool{"form_id": true, // the critic's own role form (not in the critic key)
		"prs[].owner": true} // a GitHub repo owner, not a form
	for _, p := range append(append([]string{}, RolePaths...), NotRolePaths...) {
		known[p] = true
	}
	addr := regexp.MustCompile(`(role|alert|reviewer|warn|form|owner|target|^to$|_to$)`)
	var missing []string
	for p := range paths {
		last := p[strings.LastIndex(p, ".")+1:]
		if addr.MatchString(strings.TrimSuffix(last, "[]")) && !known[p] {
			missing = append(missing, p)
		}
	}
	for p := range known {
		if !paths[p] && p != "form_id" {
			missing = append(missing, "listed but not in the model: "+p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("unclassified form-address fields: %v", missing)
	}
}

const (
	oldID = "@8d70861a00000000000000000000aaaa"
	newID = "@9e000000000000000000000000000bbb"
)

func TestRewrite(t *testing.T) {
	raw := json.RawMessage(`{"name":"s","future_field":{"x":1},"scout_watch":{"repo":"r","owners":[{"author":"a","role":"@8d70861a"},` +
		`{"author":"b","role":"@3dbeb326"},{"author":"c","role":"@8d70"}],"warn_to":"` + oldID + `"},"keel":{"alert_role":"@8d70861b"}}`)
	all := RolePaths
	pick := func(_, v string) bool { return sameForm(v, oldID) }
	next, ch, found := rewrite(raw, all, pick, newID)
	if !found || len(ch) != 2 || ch[0].Field != "scout_watch.owners[0].role" || ch[0].Old != "@8d70861a" || ch[1].Field != "scout_watch.warn_to" {
		t.Fatalf("changes %+v", ch)
	}
	s := string(next)
	if !strings.Contains(s, `"role":"`+newID+`"`) || !strings.Contains(s, `"role":"@3dbeb326"`) || !strings.Contains(s, `"role":"@8d70"`) || !strings.Contains(s, `"alert_role":"@8d70861b"`) ||
		!strings.Contains(s, `"future_field":{"x":1}`) {
		t.Fatalf("rewritten %s", s)
	}
	if again, ch, _ := rewrite(next, all, pick, newID); len(ch) != 0 || string(again) != string(next) {
		t.Fatalf("not idempotent: %v", ch)
	}
	// Alert mode on a keel watch whose alert role is empty: set.
	_, ch, found = rewrite(json.RawMessage(`{"keel":{"repo":"r"}}`), AlertPaths, func(_, v string) bool { return v != newID }, newID)
	if !found || len(ch) != 1 || ch[0].Field != "keel.alert_role" || ch[0].Old != "" {
		t.Fatalf("empty alert: %+v", ch)
	}
	if _, _, found := rewrite(json.RawMessage(`{"name":"pr"}`), AlertPaths, func(_, v string) bool { return true }, newID); found {
		t.Fatal("a PR critic has no alert role")
	}
}

func TestResolveAndRefuse(t *testing.T) {
	fs := Forms{{ID: strings.TrimPrefix(oldID, "@"), Kind: "form", TargetAria: "h1"}, {ID: strings.TrimPrefix(newID, "@"), Kind: "form"},
		{ID: "8d70861a00000000000000000000cccc", Kind: "form", TargetAria: "h2"}, {ID: "8d70861aconversation0000000000000", Kind: "conversation"}}
	if _, err := fs.resolve("@8d70861a"); err == nil || !strings.Contains(err.Error(), "2 forms share") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if a, err := fs.resolve("@8d70861a00000000000000000000aa"); err != nil || fullID(a) != oldID {
		t.Fatalf("unique prefix: %v %v", a, err)
	}
	if _, err := fs.resolve("@ffff0000"); err == nil {
		t.Fatal("a missing form resolved")
	}
	// The new role has no holder: refused before any critic is read (the store is nil).
	if _, err := Repoint(context.Background(), nil, fs, RepointReq{From: oldID, To: newID}); err == nil || !strings.Contains(err.Error(), "no target-aria") {
		t.Fatalf("holderless new role: %v", err)
	}
	if _, err := Repoint(context.Background(), nil, fs, RepointReq{From: oldID, To: "@0000aaaa"}); err == nil || !strings.Contains(err.Error(), "no such form") {
		t.Fatalf("missing new role: %v", err)
	}
	if _, err := Repoint(context.Background(), nil, fs, RepointReq{Critic: "x", To: newID}); err == nil {
		t.Fatal("mixed modes accepted")
	}
}

// Live, on scratch forms: a dry run lists exactly the addresses naming the old role and writes nothing; the run
// rewrites exactly those to the new full id; a rerun changes nothing; a new role without a holder is refused; an
// unrelated critic's form is byte-identical; --critic --alert sets one critic's alert role.
func TestLiveRepoint(t *testing.T) {
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	ctx := context.Background()
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	st, err := form.NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	mint := func(name string) string {
		id, err := f.FormNew(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
		return id
	}
	me := os.Getenv("EBAC_LIVE_HOLDER") // an aria to hold the scratch roles (nothing is sent to them)
	if me == "" {
		t.Skip("set EBAC_LIVE_HOLDER=<aria id> to hold the scratch roles")
	}
	oldRole, newRole, other, bare := mint("livetest-role-old"), mint("livetest-role-new"), mint("livetest-role-other"), mint("livetest-role-bare")
	for _, r := range []string{oldRole, newRole, other} {
		if err := f.SetJSON(ctx, r, "target-aria", me); err != nil {
			t.Fatal(err)
		}
	}
	short := oldRole[:9] // @ + 8 hex, as older configs stored it
	critic := func(name string, mod func(*model.Critic)) *model.Critic {
		id := mint("ebac-" + name)
		w := &model.Critic{Name: name + "-" + strings.TrimPrefix(id, "@")[:8], FormID: id, CreatedAt: time.Now().UTC(), EverySec: 120}
		mod(w)
		if err := st.SaveCritic(ctx, w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	kw := critic("livetest-rp-keel", func(w *model.Critic) {
		w.Source, w.Keel = "keel-threads", &model.KeelRef{Repo: "a/b", WaitingOn: []string{"x"}, AlertRole: oldRole}
	})
	sc := critic("livetest-rp-scout", func(w *model.Critic) {
		w.Source, w.Scout = model.SourceScout, &model.ScoutWatch{Repo: "a/b", Authors: []string{"x"}, WarnTo: other,
			Owners: []model.OwnerRule{{Author: "x", Role: short}, {Author: "y", Role: other}}}
	})
	kb := critic("livetest-rp-kb", func(w *model.Critic) {
		w.Source, w.Branches = model.SourceKeelBranches, &model.KeelBranches{Repo: "a/b", Reviewer: oldRole}
	})
	un := critic("livetest-rp-unrelated", func(w *model.Critic) {
		w.Source, w.Author = "github-author", &model.AuthorRef{Login: "x", Repos: []string{"a/b"}, AlertRole: other}
	})
	raw := func(id string) string {
		t.Helper()
		k, _, err := f.FormRaw(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(model.MustJSON(k))
	}
	ours := map[string]bool{kw.Name: true, sc.Name: true, kb.Name: true}
	pick := func(cs []Change) []string {
		var out []string
		for _, c := range cs {
			if ours[c.Critic] || c.Critic == un.Name {
				out = append(out, c.Critic+" "+c.Field+" "+c.Old+" -> "+c.New)
			}
		}
		sort.Strings(out)
		return out
	}
	want := []string{kb.Name + " keel_branches.reviewer " + oldRole + " -> " + newRole, kw.Name + " keel.alert_role " + oldRole + " -> " + newRole,
		sc.Name + " scout_watch.owners[0].role " + short + " -> " + newRole}
	sort.Strings(want)
	forms, err := ListForms(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	unBefore, kwBefore := raw(un.FormID), raw(kw.FormID)
	got, err := Repoint(ctx, st, forms, RepointReq{From: oldRole[:12], To: newRole, DryRun: true})
	if err != nil || strings.Join(pick(got), "\n") != strings.Join(want, "\n") {
		t.Fatalf("dry run: %v\n%s\nwant\n%s", err, strings.Join(pick(got), "\n"), strings.Join(want, "\n"))
	}
	if raw(kw.FormID) != kwBefore {
		t.Fatal("a dry run wrote")
	}
	if _, err := Repoint(ctx, st, forms, RepointReq{From: oldRole, To: bare}); err == nil || !strings.Contains(err.Error(), "no target-aria") {
		t.Fatalf("holderless new role: %v", err)
	}
	got, err = Repoint(ctx, st, forms, RepointReq{From: oldRole, To: newRole})
	if err != nil || strings.Join(pick(got), "\n") != strings.Join(want, "\n") {
		t.Fatalf("run: %v %v", err, pick(got))
	}
	load := func(w *model.Critic) *model.Critic {
		t.Helper()
		c, err := st.LoadCritic(ctx, w.Name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := load(kw); c.Keel.AlertRole != newRole || c.Keel.Repo != "a/b" || strings.Join(c.Keel.WaitingOn, ",") != "x" {
		t.Fatalf("keel %+v", c.Keel)
	}
	if c := load(sc); c.Scout.Owners[0].Role != newRole || c.Scout.Owners[1].Role != other || c.Scout.WarnTo != other {
		t.Fatalf("scout %+v", c.Scout)
	}
	if c := load(kb); c.Branches.Reviewer != newRole {
		t.Fatalf("keel-branches %+v", c.Branches)
	}
	if raw(un.FormID) != unBefore {
		t.Fatal("an unrelated critic's form moved")
	}
	if got, err := Repoint(ctx, st, forms, RepointReq{From: oldRole, To: newRole}); err != nil || len(pick(got)) != 0 {
		t.Fatalf("rerun: %v %v", err, pick(got))
	}
	// One critic's alert role.
	got, err = Repoint(ctx, st, forms, RepointReq{Critic: un.Name, Alert: newRole[:10]})
	if err != nil || len(got) != 1 || got[0].Field != "author_watch.alert_role" || load(un).Author.AlertRole != newRole {
		t.Fatalf("--alert: %v %+v", err, got)
	}
	if _, err := Repoint(ctx, st, forms, RepointReq{Critic: sc.Name, Alert: newRole}); err == nil || !strings.Contains(err.Error(), "ebac cast") {
		t.Fatalf("--alert on a critic without one: %v", err)
	}
}
