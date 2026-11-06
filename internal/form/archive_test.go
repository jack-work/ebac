package form

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/model"
	"github.com/jack-work/figaro/api/rpc"
)

// 2026-11-05: 28 archived critic forms were alive, 17 past lease, because
// every enumeration read them (a read renews keepalive) and their target-aria
// held the old seats alive.
func TestRetireKeysRenamesAndFreesTheSeat(t *testing.T) {
	cur := map[string]json.RawMessage{"name": model.MustJSON("ebac-x"), "target-aria": model.MustJSON("abc"), "kind": model.MustJSON(criticKind)}
	w := &model.Critic{Name: "x"}

	// Control: a live critic keeps its name and its seat.
	set := map[string]json.RawMessage{}
	if rm := retireKeys(w, cur, set); rm != nil || set["name"] != nil {
		t.Fatalf("live critic retired: remove=%v set=%v", rm, set)
	}

	w.Runtime.Archived = true
	set = map[string]json.RawMessage{"kind": model.MustJSON(criticKind)}
	rm := retireKeys(w, cur, set)
	if string(set["name"]) != `"ebac-archived-x"` {
		t.Fatalf("name = %s", set["name"])
	}
	if len(rm) != 1 || rm[0] != "target-aria" {
		t.Fatalf("remove = %v; want target-aria", rm)
	}
	if set["kind"] == nil {
		t.Fatal("kind dropped; it stays for history")
	}
	delete(cur, "target-aria")
	if rm := retireKeys(w, cur, map[string]json.RawMessage{}); rm != nil {
		t.Fatalf("removing an absent target-aria: %v", rm)
	}
}

func TestCriticRowsSkipsArchivedFromTheRowAlone(t *testing.T) {
	rows := []rpc.FigaroInfoResponse{
		{ID: "live", Kind: "form", Name: "ebac-x"},
		{ID: "arch", Kind: "form", Name: "ebac-archived-x"},
		{ID: "aria", Kind: "aria", Name: "ebac-y"},
		{ID: "other", Kind: "form", Name: "notes"},
	}
	got := strings.Join(criticRows(rows), ",")
	if got != "live" {
		t.Fatalf("rows read = %q; want only live", got)
	}
	// Negative control: the same retired form under its pre-archive name is
	// read, so the name is the only thing keeping it out.
	rows[1].Name = "ebac-x-old"
	if got := strings.Join(criticRows(rows), ","); got != "live,arch" {
		t.Fatalf("unrenamed archived form: rows read = %q; want live,arch", got)
	}
}

func TestValidNameRefusesTheArchivedPrefix(t *testing.T) {
	if err := model.ValidName("archived-x"); err == nil {
		t.Fatal("archived-x accepted: its form would be invisible to enumeration")
	}
	if err := model.ValidName("archivist"); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// Live: an archived form is renamed, seatless, and never read again by the
// enumeration; the same probe sees the read while the form is live.
func TestLiveArchivedFormIsNotRead(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	name := fmt.Sprintf("livetest-arch-%d", time.Now().UnixNano()%1e9)
	id, err := f.FormNew(ctx, "ebac-"+name)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })
	st := &Store{Root: t.TempDir(), Fig: f, formOf: map[string]string{}}
	w := &model.Critic{Name: name, FormID: id}
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := f.SetJSON(ctx, id, "target-aria", "0000livetest0000"); err != nil {
		t.Fatalf("set target-aria: %v", err)
	}

	enumerate := func() (string, bool) {
		var buf bytes.Buffer
		undo := figaro.AuditTo(&buf)
		defer undo()
		fresh := figaro.NewFigaro("") // dialled after AuditTo, so tapped
		defer fresh.Close()
		s := &Store{Root: st.Root, Fig: fresh, formOf: map[string]string{}}
		forms, err := s.criticForms(ctx)
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		found := false
		for _, cf := range forms {
			found = found || cf.ID == id
		}
		return buf.String(), found
	}
	if audit, found := enumerate(); !found || !strings.Contains(audit, id) {
		t.Fatalf("control: the live form was not read (found=%v):\n%s", found, audit)
	}

	w.Runtime.Archived = true
	if err := st.SaveCritic(ctx, w); err != nil {
		t.Fatalf("archive: %v", err)
	}
	keys, err := f.Form(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if keys["name"] != model.ArchivedFormPrefix+name || keys["target-aria"] != nil || keys["kind"] != criticKind {
		t.Fatalf("archived form: name=%v target-aria=%v kind=%v", keys["name"], keys["target-aria"], keys["kind"])
	}
	if audit, found := enumerate(); found || strings.Contains(audit, id) {
		t.Fatalf("the archived form was read (found=%v):\n%s", found, audit)
	}
}
