package poll

import (
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

func TestAuthorizeKeelArms(t *testing.T) {
	w := &model.Critic{Name: "keel-captains-log", Mode: model.ModeKeel, Source: "keel-threads", Write: true,
		Keel: &model.KeelRef{Repo: "notes/captains-log", Paths: []string{"2026/**", "handoffs/*.md"}}}
	const aria, role = "000000aa31ef318df6abd5ffbfe69868", "figaro-builder"
	const form = "@000000a59578e0502343dbd0f6679c7c"
	ok := func(p model.Post) model.Post {
		p.PR, p.ByAria, p.ByRole, p.ByRoleForm = "notes/captains-log", aria, role, form
		return p
	}

	accept := map[string]model.Post{
		"reply":                  ok(model.Post{ReplyTo: "t1", Body: "x"}),
		"reply + resolve":        ok(model.Post{ReplyTo: "t1", Body: "x", Resolve: true}),
		"resolve alone":          ok(model.Post{ReplyTo: "t1", Resolve: true}),
		"new thread":             ok(model.Post{Path: "2026/11/19/a.md", Line: 3, Body: "x"}),
		"new thread + rev":       ok(model.Post{Path: "handoffs/x.md", Line: 1, Rev: "abc", Body: "x"}),
		"16000 chars of 2 bytes": ok(model.Post{ReplyTo: "t1", Body: strings.Repeat("é", KeelBodyMax)}),
		// keel is internal: a body naming our tools is sent (backlog #34); the same body to a GHE PR is refused
		// (TestDisclosure's GHE arm).
		"names an internal term": ok(model.Post{ReplyTo: "t1", Body: "queued on band-hub, see the captains-log"}),
	}
	for name, p := range accept {
		if why := Authorize(w, nil, "jack", &p); why != "" {
			t.Errorf("%s refused: %s", name, why)
		}
	}
	long := ok(model.Post{ReplyTo: "t1", Body: strings.Repeat("é", KeelBodyMax+1)})
	refuse := map[string]struct {
		p    model.Post
		want string
	}{
		"other repo":        {model.Post{PR: "notes/other", ByAria: aria, ByRole: role, ByRoleForm: form, ReplyTo: "t1", Body: "x"}, "not this critic's keel repo"},
		"event":             {ok(model.Post{ReplyTo: "t1", Body: "x", Event: "APPROVE"}), "no reviews"},
		"react":             {ok(model.Post{ReplyTo: "t1", React: "+1"}), "no reactions"},
		"no writer aria":    {model.Post{PR: "notes/captains-log", ByRole: role, ByRoleForm: form, ReplyTo: "t1", Body: "x"}, "by_aria"},
		"bad writer role":   {model.Post{PR: "notes/captains-log", ByAria: aria, ByRole: "Builder", ByRoleForm: form, ReplyTo: "t1", Body: "x"}, "by_role"},
		"no role form":      {model.Post{PR: "notes/captains-log", ByAria: aria, ByRole: role, ReplyTo: "t1", Body: "x"}, "by_role_form"},
		"resolve no thread": {ok(model.Post{Resolve: true}), "resolve needs reply_to"},
		"empty body":        {ok(model.Post{ReplyTo: "t1"}), "empty body"},
		"too long":          {long, "keel takes at most 16000"},
		"path no line":      {ok(model.Post{Path: "2026/a.md", Body: "x"}), "both path and line"},
		"both kinds":        {ok(model.Post{ReplyTo: "t1", Path: "2026/a.md", Line: 1, Body: "x"}), "not both"},
		"neither kind":      {ok(model.Post{Body: "x"}), "needs reply_to"},
		"rev on a reply":    {ok(model.Post{ReplyTo: "t1", Rev: "abc", Body: "x"}), "rev anchors a new thread"},
		"path outside":      {ok(model.Post{Path: "other/a.md", Line: 1, Body: "x"}), "outside this critic's paths"},
	}
	for name, c := range refuse {
		p := c.p
		if why := Authorize(w, nil, "jack", &p); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want it to say %q", name, why, c.want)
		}
	}
	// A keel-mode critic whose source is a forge (a trusted record could split them) is still scrubbed.
	split := *w
	split.Source = "github-pr"
	if p := ok(model.Post{ReplyTo: "t1", Body: "queued on band-hub, see the captains-log"}); !strings.Contains(Authorize(&split, nil, "jack", &p), "internal term") {
		t.Errorf("keel mode + github-pr source: a body naming band-hub was not refused")
	}
	ro := *w
	ro.Write = false
	p := ok(model.Post{ReplyTo: "t1", Body: "x"})
	if why := Authorize(&ro, nil, "jack", &p); !strings.Contains(why, "read-only") {
		t.Errorf("read-only keel critic: %q", why)
	}
}
