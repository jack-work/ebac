package trigger

import (
	"encoding/json"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

func TestGlobMatchesSegmentsAndDoubleStarAnywhere(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/decisions-lead/**", "2026/11/17/decisions-lead/a.md", true},
		{"**/decisions-lead/**", "2026/11/17/decisions-lead/x/y.md", true},
		{"**/decisions-lead/**", "2026/11/17/decisions-leads/a.md", false},
		{"**/decisions-lead/**", "decisions-lead.md", false},
		{"**/ebac-*.md", "2026/11/19/ebac-journal-errors.md", true},
		{"**/ebac-*.md", "ebac-top.md", true},
		{"**/ebac-*.md", "2026/11/19/ebac-rework/PLAN.md", false},
		{"handoffs/*.md", "handoffs/ebac-builder.md", true},
		{"handoffs/*.md", "handoffs/old/ebac-builder.md", false},
		{"a/**", "a", true},
		{"a/**", "ab/c", false},
		{"**", "x", true},
		{"**", "", false},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/b/c", false},
	}
	for _, c := range cases {
		if got := Glob(c.glob, c.path); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestCheckGlobRefusesWhatGlobCannotApply(t *testing.T) {
	for _, g := range []string{"", "  ", "a**/b", "x/[", "**b"} {
		if err := CheckGlob(g); err == nil {
			t.Errorf("CheckGlob(%q) accepted", g)
		}
	}
	for _, g := range []string{"**/x/**", "a/*.md", "handoffs/*.md"} {
		if err := CheckGlob(g); err != nil {
			t.Errorf("CheckGlob(%q): %v", g, err)
		}
	}
}

func TestParseRefusesBadRoutes(t *testing.T) {
	bad := map[string]string{
		"to without @":           `[{"path":"a/**","to":"000000a1","action":"keep"},{"default":true,"action":"keep"}]`,
		"to with drop":           `[{"path":"a/**","to":"@000000a1","action":"drop"},{"default":true,"action":"keep"}]`,
		"role_in_path with to":   `[{"role_in_path":true,"to":"@000000a1","action":"keep"},{"default":true,"action":"keep"}]`,
		"role_in_path with drop": `[{"role_in_path":true,"action":"drop"},{"default":true,"action":"keep"}]`,
		"bad path glob":          `[{"path":"a**/b","to":"@000000a1","action":"keep"},{"default":true,"action":"keep"}]`,
		"bare @":                 `[{"path":"a/**","to":"@","action":"keep"},{"default":true,"action":"keep"}]`,
	}
	for name, s := range bad {
		if _, err := Parse(json.RawMessage(s)); err == nil {
			t.Errorf("%s: accepted %s", name, s)
		}
	}
}

// The captains-log critic's set: explicit rows first, then a path that names a
// role, then the default owner.
const routeSet = `[{"contains":["jack via Figaro "],"action":"record"},
 {"path":["**/ebac-rework/**","**/github-cache/**","**/object-model/**","**/ebac-*.md"],"to":"@000000a1","action":"keep"},
 {"path":"**/decisions-lead/**","to":"@000000a6","action":"keep"},
 {"role_in_path":true,"action":"keep"},
 {"default":true,"to":"@000000a4","action":"keep"}]`

var roster = map[string]string{"flake-owner": "@000000a3", "figaro-owner": "@000000a2", "ebac-owner": "@000000a1"}

func routeOf(t *testing.T, rs Rules, path string) (string, model.Tier) {
	t.Helper()
	kept, dropped := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Tier: model.TierWake, Path: path, Text: "please fix"}}, roster)
	if len(dropped) != 0 || len(kept) != 1 {
		t.Fatalf("%s: kept %d dropped %d", path, len(kept), len(dropped))
	}
	return kept[0].Route, kept[0].Tier
}

func TestRoutesExplicitThenRoleInPathThenDefault(t *testing.T) {
	rs := mustParse(t, routeSet)
	cases := map[string]string{
		"2026/11/19/ebac-rework/PLAN.md":              "@000000a1", // explicit row
		"2026/11/19/github-cache/DESIGN.md":           "@000000a1",
		"2026/11/19/ebac-journal-errors.md":           "@000000a1", // explicit **/ebac-*.md
		"2026/11/17/decisions-lead/handoff-topo.md":   "@000000a6",
		"2026/11/19/flake-owner/notes.md":             "@000000a3", // derived: a directory names a role
		"handoffs/figaro-owner.md":                    "@000000a2", // derived: the file names a role
		"2026/11/19/peers/mined.md":                   "@000000a4", // "peers" is not a role: default
		"2026/11/19/flake-owner/ebac-rework/x.md":     "@000000a1", // explicit beats derived
		"2026/11/19/figaro-owner/flake-owner/plan.md": "@000000a3", // the deepest role segment wins
	}
	for path, want := range cases {
		if got, tier := routeOf(t, rs, path); got != want || tier != model.TierWake {
			t.Errorf("%s: route %q tier %v, want %q wake", path, got, tier, want)
		}
	}
}

func TestRouteNoneWhenRolesUnknownFallsToDefault(t *testing.T) {
	rs := mustParse(t, routeSet)
	kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Tier: model.TierWake, Path: "handoffs/figaro-owner.md"}}, nil)
	if kept[0].Route != "@000000a4" {
		t.Fatalf("route %q, want the default @000000a4 when no role names are known", kept[0].Route)
	}
}

func TestRuleWithoutToClearsAnEarlierRoute(t *testing.T) {
	rs := mustParse(t, `[{"default":true,"action":"keep"}]`)
	kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Route: "@stale"}}, nil)
	if kept[0].Route != "" {
		t.Fatalf("route %q, want empty (the critic's own alert target)", kept[0].Route)
	}
}

func TestRecordRowStillWinsOverRouting(t *testing.T) {
	rs := mustParse(t, routeSet)
	kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Tier: model.TierWake, Path: "2026/11/19/ebac-rework/PLAN.md",
		Text: "done (jack via Figaro 0.77)"}}, roster)
	if kept[0].Tier != model.TierRecord || kept[0].Route != "" {
		t.Fatalf("tier %v route %q, want record with no route", kept[0].Tier, kept[0].Route)
	}
}

func TestRoleInPathDeepestSegmentAndFileName(t *testing.T) {
	cases := map[string]string{
		"handoffs/ebac-owner.md":            "@000000a1",
		"a/flake-owner/b/figaro-owner/c.md": "@000000a2",
		"a/flake-owner.md/x.md":             "",
		"notes/flake-owner.txt":             "",
		"":                                  "",
		"2026/11/19/not-a-role/also-not-a-role.md": "",
	}
	for p, want := range cases {
		if got := RoleInPath(p, roster); got != want {
			t.Errorf("RoleInPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestStampRoleRoutesFindingsToTheBuilderAndAnswersToThePool(t *testing.T) {
	rs := mustParse(t, `[{"stamp_role":"reviewer","to":"@000000a9","action":"wake"},
 {"stamp_role":"builder","to":"@000000a5","action":"wake"},
 {"default":true,"action":"keep"}]`)
	cases := map[string]string{
		"Figaro 000000aa (reviewer) ⟩ jack":                  "@000000a9",
		"Figaro aa758aa465de4f91b483184ab59a0e59 (reviewer)": "@000000a9",
		"Figaro 000000a9 (builder) ⟩ jack":                   "@000000a5",
		"Figaro 000000aa (system1-engineer) ⟩ jack":          "",
		"jack ⟩ jack":                       "",
		"Someone (reviewer) ⟩ jack":         "",
		"Figaro zzzzzzzz (reviewer) ⟩ jack": "",
	}
	for author, want := range cases {
		kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Author: author}}, nil)
		if kept[0].Route != want {
			t.Errorf("%q: route %q, want %q", author, kept[0].Route, want)
		}
	}
	if StampRole("Figaro 000000aa (system1-engineer) ⟩ jack") != "system1-engineer" {
		t.Errorf("digit role not read")
	}
}

// A critic skips figaro posts by author, not by a body stamp the door may
// stop writing.
func TestStampRoleStarMatchesAnyFigaroAuthorAndNoOneElse(t *testing.T) {
	rs := mustParse(t, `[{"stamp_role":"*","action":"drop"},
 {"stamp_role":"checker","to":"@first","action":"wake"},
 {"default":true,"to":"@owner","action":"wake"}]`)
	cases := []struct{ author, text, want string }{
		{"Figaro 1a2b3c4d (checker) ⟩ jack", "a finding, no body stamp", "dropped"},
		{"Figaro 5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b (ops-lead)", "no stamp", "dropped"},
		{"Figaro 0f1e2d3c (builder) ⟩ jack", "jack via Figaro 0f1e2d3c (builder): old stamp", "dropped"},
		{"jack ⟩ jack", "the Boss writes", "@owner"},
		{"Someone (checker) ⟩ jack", "not a figaro", "@owner"},
		{"Figaro zzzzzzzz (checker) ⟩ jack", "bad aria id", "@owner"},
		{"Figaro 1a2b3c4d (*) ⟩ jack", "a literal star is no role", "@owner"},
		{"", "no author", "@owner"},
	}
	for _, c := range cases {
		kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Author: c.author, Text: c.text}}, nil)
		got := "dropped"
		if len(kept) == 1 {
			got = kept[0].Route
		}
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.author, got, c.want)
		}
	}
}

func TestAListedStampRoleIsUnchangedBesideAStar(t *testing.T) {
	rs := mustParse(t, `[{"stamp_role":"checker","to":"@first","action":"wake"},
 {"stamp_role":"*","to":"@pool","action":"wake"},
 {"default":true,"action":"keep"}]`)
	for author, want := range map[string]string{
		"Figaro 1a2b3c4d (checker) ⟩ jack": "@first",
		"Figaro 0f1e2d3c (builder) ⟩ jack": "@pool",
		"jack ⟩ jack":                      "",
	} {
		kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Author: author}}, nil)
		if kept[0].Route != want {
			t.Errorf("%q: route %q, want %q", author, kept[0].Route, want)
		}
	}
}
