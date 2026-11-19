package trigger

import (
	"encoding/json"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func TestGlobMatchesSegmentsAndDoubleStarAnywhere(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/decisions-minister/**", "2026/11/17/decisions-minister/a.md", true},
		{"**/decisions-minister/**", "2026/11/17/decisions-minister/x/y.md", true},
		{"**/decisions-minister/**", "2026/11/17/decisions-ministers/a.md", false},
		{"**/decisions-minister/**", "decisions-minister.md", false},
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
		"to without @":           `[{"path":"a/**","to":"6fa9788a","action":"keep"},{"default":true,"action":"keep"}]`,
		"to with drop":           `[{"path":"a/**","to":"@6fa9788a","action":"drop"},{"default":true,"action":"keep"}]`,
		"role_in_path with to":   `[{"role_in_path":true,"to":"@6fa9788a","action":"keep"},{"default":true,"action":"keep"}]`,
		"role_in_path with drop": `[{"role_in_path":true,"action":"drop"},{"default":true,"action":"keep"}]`,
		"bad path glob":          `[{"path":"a**/b","to":"@6fa9788a","action":"keep"},{"default":true,"action":"keep"}]`,
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
 {"path":["**/ebac-rework/**","**/github-cache/**","**/object-model/**","**/ebac-*.md"],"to":"@6fa9788a","action":"keep"},
 {"path":"**/decisions-minister/**","to":"@2ea091e5","action":"keep"},
 {"role_in_path":true,"action":"keep"},
 {"default":true,"to":"@d407b690","action":"keep"}]`

var roster = map[string]string{"flake-steward": "@9d5cda1d", "figaro-steward": "@e44cf463", "ebac-steward": "@6fa9788a"}

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
		"2026/11/19/ebac-rework/PLAN.md":                  "@6fa9788a", // explicit row
		"2026/11/19/github-cache/DESIGN.md":               "@6fa9788a",
		"2026/11/19/ebac-journal-errors.md":               "@6fa9788a", // explicit **/ebac-*.md
		"2026/11/17/decisions-minister/handoff-topo.md":   "@2ea091e5",
		"2026/11/19/flake-steward/notes.md":               "@9d5cda1d", // derived: a directory names a role
		"handoffs/figaro-steward.md":                      "@e44cf463", // derived: the file names a role
		"2026/11/19/peers/mined.md":                       "@d407b690", // "peers" is not a role: default
		"2026/11/19/flake-steward/ebac-rework/x.md":       "@6fa9788a", // explicit beats derived
		"2026/11/19/figaro-steward/flake-steward/plan.md": "@9d5cda1d", // the deepest role segment wins
	}
	for path, want := range cases {
		if got, tier := routeOf(t, rs, path); got != want || tier != model.TierWake {
			t.Errorf("%s: route %q tier %v, want %q wake", path, got, tier, want)
		}
	}
}

func TestRouteNoneWhenRolesUnknownFallsToDefault(t *testing.T) {
	rs := mustParse(t, routeSet)
	kept, _ := rs.Apply([]model.Event{{Kind: model.EvCommentNew, Tier: model.TierWake, Path: "handoffs/figaro-steward.md"}}, nil)
	if kept[0].Route != "@d407b690" {
		t.Fatalf("route %q, want the default @d407b690 when no role names are known", kept[0].Route)
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
		"handoffs/ebac-steward.md":              "@6fa9788a",
		"a/flake-steward/b/figaro-steward/c.md": "@e44cf463",
		"a/flake-steward.md/x.md":               "",
		"notes/flake-steward.txt":               "",
		"":                                      "",
		"2026/11/19/not-a-role/also-not-a-role.md": "",
	}
	for p, want := range cases {
		if got := RoleInPath(p, roster); got != want {
			t.Errorf("RoleInPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestStampRoleRoutesFindingsToTheKnightAndAnswersToThePool(t *testing.T) {
	rs := mustParse(t, `[{"stamp_role":"reviewer","to":"@6d1ebf69","action":"wake"},
 {"stamp_role":"knight","to":"@8292f32b","action":"wake"},
 {"default":true,"action":"keep"}]`)
	cases := map[string]string{
		"Figaro e680885d (reviewer) ⟩ jack":                  "@6d1ebf69",
		"Figaro aa758aa465de4f91b483184ab59a0e59 (reviewer)": "@6d1ebf69",
		"Figaro 6d1ebf69 (knight) ⟩ jack":                    "@8292f32b",
		"Figaro e680885d (system1-engineer) ⟩ jack":          "",
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
	if StampRole("Figaro e680885d (system1-engineer) ⟩ jack") != "system1-engineer" {
		t.Errorf("digit role not read")
	}
}
