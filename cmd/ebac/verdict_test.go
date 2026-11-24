package main

import (
	"fmt"
	"github.com/gluck/ebac/sdk/model"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/identity"
)

func TestVerdictGuardArms(t *testing.T) {
	a := func(n int) string { return fmt.Sprintf("%032x", n) }
	rf, kf := "@000000a59578e0502343dbd0f6679c7c", "@000000a900000000000000000000000a"
	// 1 holds reviewer, 9 holds builder (the author), 10 is a fork of 9 holding nothing.
	d := identity.Directory{
		Forms:  map[string][]string{"reviewer": {rf}, "builder": {kf}},
		Holder: map[string]string{rf: a(1), kf: a(9)},
		Parent: map[string]string{a(1): "", a(9): "", a(10): a(9)},
	}
	cfg := model.KeelReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer"}, AuthorRole: "builder"}
	if form, err := verdictGuard(d, a(1), "reviewer", "r9", "r9", cfg); err != nil || form != rf {
		t.Fatalf("holder at the current rev: %q %v", form, err)
	}
	recastAuthor := d
	recastAuthor.Holder = map[string]string{rf: a(1), kf: a(1)} // the reviewer also holds the author role
	cases := []struct {
		name, caller, role, rev, current string
		d                                identity.Directory
		want                             string
	}{
		{"no caller", "", "reviewer", "r9", "r9", d, "no caller aria"},
		{"not a reviewer of this review", a(1), "builder", "r9", "r9", d, "is not a reviewer of this review"},
		{"no revision on keel", a(1), "reviewer", "r9", "", d, "keel has no revision"},
		{"reviewed an older rev", a(1), "reviewer", "r8", "r9", d, `--rev "r8" is not the doc's current revision r9`},
		{"a fork of the holder", a(10), "reviewer", "r9", "r9", d, "is not the holder of role reviewer"},
		{"the author side", a(1), "reviewer", "r9", "r9", recastAuthor, "is the author side"},
	}
	for _, c := range cases {
		if _, err := verdictGuard(c.d, c.caller, c.role, c.rev, c.current, cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestReviewGuardArms(t *testing.T) {
	a := func(n int) string { return fmt.Sprintf("%032x", n) }
	rf, kf := "@000000a59578e0502343dbd0f6679c7c", "@000000a900000000000000000000000a"
	d := identity.Directory{
		Forms:  map[string][]string{"reviewer": {rf}, "builder": {kf}},
		Holder: map[string]string{rf: a(1), kf: a(9)},
		Parent: map[string]string{a(1): "", a(9): "", a(10): a(9)},
	}
	if err := reviewGuard(d, a(9), "builder", []string{"reviewer"}); err != nil {
		t.Fatalf("author holder: %v", err)
	}
	if err := reviewGuard(d, a(10), "builder", []string{"reviewer"}); err != nil {
		t.Fatalf("fork of the author holder: %v", err)
	}
	cases := []struct {
		name, caller, author string
		reviewers            []string
		want                 string
	}{
		{"no caller", "", "builder", []string{"reviewer"}, "no caller aria"},
		{"not the author side", a(1), "builder", []string{"reviewer"}, "only the author role's holder"},
		{"no reviewers", a(9), "builder", nil, "are required"},
		{"unknown reviewer", a(9), "builder", []string{"gatekeeper"}, `role "gatekeeper": no live role form`},
		{"author as reviewer", a(9), "builder", []string{"reviewer", "builder"}, "cannot be a reviewer"},
	}
	for _, c := range cases {
		if err := reviewGuard(d, c.caller, c.author, c.reviewers); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
