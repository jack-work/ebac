package poll

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/source/keel"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// noThreads answers keel's threads read with none.
type noThreads struct{}

func (noThreads) Where() string { return "none" }
func (noThreads) Get(context.Context, string, time.Duration) (int, []byte, error) {
	return 200, []byte(`{"paths":{}}`), nil
}

type countWriter struct{ n int }

func (c *countWriter) Write(context.Context, string, string, string, string, []byte) (int, []byte, error) {
	c.n++
	return 200, []byte(`{"ok":true}`), nil
}

const (
	holderAria = "aa758aa465de4f91b483184ab59a0e59"
	poolForm   = "@8292f32b9578e0502343dbd0f6679c7c"
)

func TestRecheckKeelRolesArms(t *testing.T) {
	post := func() *model.Post {
		return &model.Post{PR: "notes/captains-log", ReplyTo: "t1", Body: "x", ByAria: holderAria, ByRole: "reviewer", ByRoleForm: poolForm}
	}
	cases := []struct {
		name            string
		form, why       string
		err             error
		refused, errSub string
		kept            bool
	}{
		{"still held", poolForm, "", nil, "", "", true},
		{"figaro down: transient", "", "", errors.New("list arias: connection refused"), "", "role check: list arias: connection refused", false},
		{"succession on the same form", poolForm, "caller x is not the holder", nil, "changed since the post was written (succession)", "", false},
		{"role moved to another form", "@0000000000000000000000000000beef", "", nil, "moved to another role form", "", false},
		{"not held, form unresolved", "", "role \"reviewer\" is not on the roster", nil, "is not on the roster", "", false},
	}
	for _, c := range cases {
		p := &Poller{Out: io.Discard, RoleCheck: func(context.Context, string, string) (string, string, error) { return c.form, c.why, c.err }}
		pp := post()
		todo := map[string]*model.Post{"p": pp}
		p.recheckKeelRoles(context.Background(), &model.Critic{Name: "k", Mode: model.ModeKeel}, todo)
		_, kept := todo["p"]
		if kept != c.kept || !strings.Contains(pp.Refused, c.refused) || (c.refused == "" && pp.Refused != "") ||
			!strings.Contains(pp.Error, c.errSub) || (c.errSub == "" && pp.Error != "") {
			t.Errorf("%s: kept %v refused %q error %q", c.name, kept, pp.Refused, pp.Error)
		}
	}
}

// The flush is the enforcing point: a post written by hand onto the form,
// naming a holder, is refused there and never reaches keel.
func TestLiveHandWrittenKeelPostIsRefusedAtTheFlush(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, w, snap := liveCritic(t, f, true)
	w.Mode, w.Source, w.Keel = model.ModeKeel, "keel-threads", &model.KeelRef{Repo: "notes/captains-log"}
	if err := st.SaveRecordingForTest(ctx, w, snap, nil); err != nil {
		t.Fatal(err)
	}
	cw := &countWriter{}
	p := &Poller{Store: st, Fig: f, Out: os.Stderr, Me: "jack",
		Plugins: plugin.InProc{"keel": keel.Plugin{Client: &keel.Client{T: noThreads{}}, Writer: cw}.Handle},
		RoleCheck: func(context.Context, string, string) (string, string, error) {
			return poolForm, "caller " + holderAria + " is not the holder of role reviewer", nil
		}}
	writePost(t, st, w, "hand", model.Post{PR: "notes/captains-log", ReplyTo: "t1", Body: "claimed",
		ByAria: holderAria, ByRole: "reviewer", ByRoleForm: poolForm})
	acks := p.FlushOutbox(ctx, w, snap)
	if err := st.SaveRecordingForTest(ctx, w, snap, acks); err != nil {
		t.Fatal(err)
	}
	got := readPost(t, f, w, "hand")
	if cw.n != 0 || !strings.Contains(got.Refused, "(succession)") || got.Sent != nil {
		b, _ := json.Marshal(got)
		t.Fatalf("writes %d post %s", cw.n, b)
	}
}

// The same enforcement without a daemon (default suite): post() re-checks a
// keel post's role before the plugin sees it.
func TestPostRechecksKeelRolesBeforeThePlugin(t *testing.T) {
	cw := &countWriter{}
	p := &Poller{Out: io.Discard,
		Plugins: plugin.InProc{"keel": keel.Plugin{Client: &keel.Client{T: noThreads{}}, Writer: cw}.Handle},
		RoleCheck: func(context.Context, string, string) (string, string, error) {
			return poolForm, "caller " + holderAria + " is not the holder of role reviewer", nil
		}}
	w := &model.Critic{Name: "k", Mode: model.ModeKeel, Source: "keel-threads", Write: true, Keel: &model.KeelRef{Repo: "notes/captains-log"}}
	post := &model.Post{PR: "notes/captains-log", ReplyTo: "t1", Body: "claimed", ByAria: holderAria, ByRole: "reviewer", ByRoleForm: poolForm}
	p.post(context.Background(), w, nil, map[string]*model.Post{"hand": post})
	if cw.n != 0 || !strings.Contains(post.Refused, "(succession)") || post.Sent != nil {
		t.Fatalf("writes %d post %+v", cw.n, post)
	}
}
