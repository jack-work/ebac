package form

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func TestStripLocal(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"scout removed, siblings kept",
			`{"other":1,"ebac":{"snapshot_round":3,"runtime":{"round":7,"scout":{"cursor":"c"}}}}`,
			`{"ebac":{"runtime":{"round":7},"snapshot_round":3},"other":1}`},
		{"no scout: untouched", `{"ebac":{"runtime":{"round":7}}}`, `{"ebac":{"runtime":{"round":7}}}`},
		{"no runtime: untouched", `{"ebac":{"snapshot_round":1}}`, `{"ebac":{"snapshot_round":1}}`},
		{"no ebac: untouched", `{"other":1}`, `{"other":1}`},
	} {
		set := map[string]json.RawMessage{"system": json.RawMessage(c.in), "critic": json.RawMessage(`{"name":"x"}`)}
		stripLocal(set)
		if got := string(set["system"]); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
		if string(set["critic"]) != `{"name":"x"}` {
			t.Errorf("%s: critic touched", c.name)
		}
	}
	// A set without system gains none.
	set := map[string]json.RawMessage{"critic": json.RawMessage(`{}`)}
	stripLocal(set)
	if _, ok := set["system"]; ok {
		t.Fatal("stripLocal added a system key")
	}
}

func TestLocalRoundTripAndGuards(t *testing.T) {
	var warn bytes.Buffer
	s := &Store{Root: t.TempDir(), Warn: &warn}
	const id = "@6f376a71c0155c8738886f0895b64418"
	if got := s.readLocal(id); got != nil {
		t.Fatalf("absent file read as %s", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("an absent file is not a warning: %q", warn.String())
	}
	state := json.RawMessage(`{"cursor":"2026-11-12T00:00:00Z","prs":{"1":{"s":"OPEN"}}}`)
	if err := s.writeLocal(id, state); err != nil {
		t.Fatal(err)
	}
	if got := s.readLocal(id); string(got) != string(state) {
		t.Fatalf("round trip: %s", got)
	}
	if p := s.localPath(id); !strings.HasPrefix(p, filepath.Join(s.Root, "runtime")) || strings.Contains(filepath.Base(p), "@") {
		t.Fatalf("path %s", p)
	}
	// Empty state never overwrites a saved one.
	if err := s.writeLocal(id, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.readLocal(id); string(got) != string(state) {
		t.Fatalf("an empty write wiped the state: %s", got)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(filepath.Join(s.Root, "runtime"))
	if len(ents) != 1 {
		t.Fatalf("runtime dir holds %d files, want 1", len(ents))
	}
	// A corrupt file reads as none, and says so.
	if err := os.WriteFile(s.localPath(id), []byte(`{"cursor":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.readLocal(id); got != nil {
		t.Fatalf("corrupt file read as %s", got)
	}
	if !strings.Contains(warn.String(), "not JSON") {
		t.Fatalf("no warning for a corrupt file: %q", warn.String())
	}
	// An unwritable root fails the write (the caller warns; the round goes on).
	ro := &Store{Root: filepath.Join(s.Root, "file-not-dir")}
	if err := os.WriteFile(ro.Root, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ro.writeLocal(id, state); err == nil {
		t.Fatal("a write under a file succeeded")
	}
}

func TestEbacOverlaysLocal(t *testing.T) {
	keys := map[string]json.RawMessage{"system": json.RawMessage(`{"ebac":{"runtime":{"round":4,"scout":{"cursor":"form"}}}}`)}
	// Migration: no file, the form's copy is the state.
	f := &CriticForm{Keys: keys}
	if got := string(f.ebac().Runtime.Scout); got != `{"cursor":"form"}` {
		t.Fatalf("no file: %s", got)
	}
	// The file wins over a stale form copy; other runtime fields are the form's.
	f.local = json.RawMessage(`{"cursor":"disk"}`)
	e := f.ebac()
	if string(e.Runtime.Scout) != `{"cursor":"disk"}` || e.Runtime.Round != 4 {
		t.Fatalf("overlay: scout %s round %d", e.Runtime.Scout, e.Runtime.Round)
	}
	// The overlay never touches the keys Commit diffs against.
	if !strings.Contains(string(f.Keys["system"]), `"form"`) {
		t.Fatal("overlay rewrote the form keys")
	}
}

// Live, through the daemon: the scout state leaves the form for the disk on
// save, comes back on load, migrates once from a form that still carries it,
// and no Commit builder can put it back. The control: the same write with
// the strip off (ebac before this change) DOES put it on the form, and the
// probe sees it.
func TestLiveScoutStateLivesOnDisk(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	st, err := NewStore(f)
	if err != nil {
		t.Fatal(err)
	}
	st.Root = t.TempDir()
	id, err := f.FormNew(ctx, "ebac-livetest-local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	name := "livetest-local-" + strings.TrimPrefix(id, "@")[:8]
	onForm := func() string { // the scout member on the live form, "" when absent
		t.Helper()
		cf, err := st.ReadForm(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var sys struct {
			Ebac struct {
				Runtime map[string]json.RawMessage `json:"runtime"`
			} `json:"ebac"`
		}
		_ = json.Unmarshal(cf.Keys["system"], &sys)
		return string(sys.Ebac.Runtime["scout"])
	}
	w := &model.Critic{Name: name, FormID: id, Source: "keel-threads", CreatedAt: time.Now().UTC(), EverySec: 60,
		Keel: &model.KeelRef{Repo: "notes/x"}}
	w.Runtime.Scout = json.RawMessage(`{"cursor":"a"}`)
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := onForm(); got != "" {
		t.Fatalf("save put the scout state on the form: %s", got)
	}
	if got := string(st.readLocal(id)); got != `{"cursor":"a"}` {
		t.Fatalf("disk after save: %q", got)
	}
	c, err := st.LoadCritic(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Runtime.Scout) != `{"cursor":"a"}` {
		t.Fatalf("load: %s", c.Runtime.Scout)
	}

	// Migration: the file is gone and the form carries the state (written raw,
	// past Commit, as every form did before this change).
	if err := os.Remove(st.localPath(id)); err != nil {
		t.Fatal(err)
	}
	stripScout = func(map[string]json.RawMessage) {} // ebac before this change
	err = st.Commit(ctx, id, func(cur map[string]json.RawMessage, sys EbacState) (map[string]json.RawMessage, []string) {
		sys.Runtime.Scout = json.RawMessage(`{"cursor":"m"}`)
		system := model.ObjectOf(cur["system"])
		system["ebac"] = model.MustJSON(sys)
		return map[string]json.RawMessage{"system": model.MustJSON(system)}, nil
	})
	stripScout = stripLocal
	if err != nil {
		t.Fatal(err)
	}
	if got := onForm(); got != `{"cursor":"m"}` {
		t.Fatalf("control: the raw write did not land, the probe proves nothing: %q", got)
	}
	c, err = st.LoadCritic(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Runtime.Scout) != `{"cursor":"m"}` {
		t.Fatalf("migration load: %s", c.Runtime.Scout)
	}
	if err := st.SaveCritic(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := onForm(); got != "" {
		t.Fatalf("migration save left the state on the form: %s", got)
	}
	if got := string(st.readLocal(id)); got != `{"cursor":"m"}` {
		t.Fatalf("migration disk: %q", got)
	}

	// A foreign Commit builder writing system back with the state: stripped.
	if err := st.Commit(ctx, id, func(cur map[string]json.RawMessage, sys EbacState) (map[string]json.RawMessage, []string) {
		sys.Runtime.Scout = json.RawMessage(`{"cursor":"leak"}`)
		system := model.ObjectOf(cur["system"])
		system["ebac"] = model.MustJSON(sys)
		return map[string]json.RawMessage{"system": model.MustJSON(system)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := onForm(); got != "" {
		t.Fatalf("a Commit builder put the state on the form: %s", got)
	}
	if got := string(st.readLocal(id)); got != `{"cursor":"m"}` {
		t.Fatalf("a foreign Commit moved the disk state: %q", got)
	}

	// A save whose commit is refused writes nothing to disk: the file is
	// never ahead of the form. The guard refuses renaming the critic.
	bad := *c
	bad.Name = name + "-renamed"
	bad.Runtime.Scout = json.RawMessage(`{"cursor":"ahead"}`)
	if err := st.SaveCritic(ctx, &bad); err == nil {
		t.Fatal("control: the renaming save was not refused, the order check proves nothing")
	}
	if got := string(st.readLocal(id)); got != `{"cursor":"m"}` {
		t.Fatalf("a refused save moved the disk state ahead of the form: %q", got)
	}
}
