package poll

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/model"
)

func TestCheckVerdictCountsOnlyAStampedBoundVerdictOfTheCurrentHolder(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	w := &model.Critic{Name: "k", FormID: "@7d99311ef30a41de02b53090bd27a2e8", Keel: &model.KeelRef{Repo: "notes/captains-log"}}
	cfg := model.KeelReviewConfig{Doc: "d.md", Reviewers: []string{"reviewer"}}
	form, holder := "@8292f32b9578e0502343dbd0f6679c7c", fmt.Sprintf("%032x", 1)
	d := identity.Directory{Forms: map[string][]string{"reviewer": {form}}, Holder: map[string]string{form: holder},
		Parent: map[string]string{holder: ""}}
	stamped := func(mut func(v *model.KeelVerdict)) []byte {
		v := model.KeelVerdict{Verdict: "pass", Rev: "r9", At: "t", ByAria: holder, Repo: "notes/captains-log", Doc: "d.md", RoleForm: form}
		mut(&v)
		v.Stamp = identity.Stamp(key, model.KeelVerdictStampFields(w.FormID, w.Name, "reviewer", v)...)
		return model.MustJSON(v)
	}
	ok := stamped(func(*model.KeelVerdict) {})
	if c := checkVerdict(key, w, cfg, "reviewer", ok, d, nil); !c.Valid {
		t.Fatalf("good verdict: %+v", c)
	}
	moved := d
	moved.Forms = map[string][]string{"reviewer": {"@0000000000000000000000000000beef"}}
	moved.Holder = map[string]string{"@0000000000000000000000000000beef": holder}
	recast := d
	recast.Holder = map[string]string{form: fmt.Sprintf("%032x", 2)}
	edited := []byte(strings.Replace(string(ok), `"verdict":"pass"`, `"verdict":"changes"`, 1))
	cases := []struct {
		name string
		raw  []byte
		d    identity.Directory
		derr error
		why  string
	}{
		{"no verdict", nil, d, nil, "no verdict"},
		{"hand-set, no stamp", []byte(`{"verdict":"pass","rev":"r9"}`), d, nil, "unstamped or edited"},
		{"edited after stamping", edited, d, nil, "unstamped or edited"},
		{"validly stamped for another doc", stamped(func(v *model.KeelVerdict) { v.Doc = "x.md" }), d, nil, "another repo or doc"},
		{"validly stamped for another repo", stamped(func(v *model.KeelVerdict) { v.Repo = "notes/x" }), d, nil, "another repo or doc"},
		{"role moved to another form", ok, moved, nil, "the role is now @0000000000000000000000000000beef"},
		{"role recast to another holder (G1)", ok, recast, nil, "re-verdict"},
		{"figaro unreadable", ok, d, errors.New("list arias: down"), "role check: list arias: down"},
	}
	for _, c := range cases {
		got := checkVerdict(key, w, cfg, "reviewer", c.raw, c.d, c.derr)
		if got.Valid || !strings.Contains(got.Why, c.why) {
			t.Errorf("%s: %+v, want invalid %q", c.name, got, c.why)
		}
	}
}

func TestBuildDirectoryFromOneListing(t *testing.T) {
	a := func(n int) string { return fmt.Sprintf("%032x", n) }
	d := BuildDirectory([]model.Aria{
		{ID: a(1), Kind: "conversation"},
		{ID: a(2), Kind: "conversation", Parent: a(1)},
		{ID: a(3), Kind: "conversation", Parent: a(1), ReapedAt: 1794900000000},
		{ID: "@8292f32b9578e0502343dbd0f6679c7c", Kind: "form", Name: "reviewer", TargetAria: a(1)},
		{ID: "6d1ebf6900000000000000000000000a", Kind: "form", Name: "knight", TargetAria: a(2)},
		{ID: "@0000000000000000000000000000dead", Kind: "form", Name: "reviewer~moved", TargetAria: a(2)},
		{ID: "@000000000000000000000000000000aa", Kind: "form", Name: "gone", ReapedAt: 1},
	})
	if d.Parent[a(3)] != a(1) || !d.Reaped[a(3)] {
		t.Errorf("a reaped aria keeps its parent link and is marked reaped: %v %v", d.Parent, d.Reaped)
	}
	if _, ok, why := d.MayPost(a(3), "reviewer"); ok || !strings.Contains(why, "is reaped") {
		t.Errorf("reaped caller accepted: %v %q", ok, why)
	}
	if d.Parent[a(2)] != a(1) || len(d.Parent) != 3 {
		t.Errorf("parents %v", d.Parent)
	}
	if got := d.Forms["reviewer"]; len(got) != 1 || got[0] != "@8292f32b9578e0502343dbd0f6679c7c" {
		t.Errorf("reviewer forms %v", got)
	}
	if got := d.Forms["knight"]; len(got) != 1 || got[0] != "@6d1ebf6900000000000000000000000a" || d.Holder[got[0]] != a(2) {
		t.Errorf("knight form %v holder %v", got, d.Holder)
	}
	if _, ok := d.Forms["reviewer~moved"]; ok || len(d.Forms["gone"]) != 0 {
		t.Errorf("moved or reaped form named: %v", d.Forms)
	}
}
