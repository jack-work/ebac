package form

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/gluck/ebac/internal/figaro"
)

// Live, on a scratch form: does a Commit that rewrites a top-level object
// land member-wise (sibling kept, nested, no flat dotted key)?
func TestLiveCommitMemberwise(t *testing.T) {
	id := os.Getenv("EBAC_SCRATCH_FORM")
	if id == "" {
		t.Skip("EBAC_SCRATCH_FORM unset")
	}
	st, err := NewStore(figaro.NewFigaro(""))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = st.Commit(ctx, id, func(cur map[string]json.RawMessage, _ EbacState) (map[string]json.RawMessage, []string) {
		var box map[string]map[string]any
		_ = json.Unmarshal(cur["inbox"], &box)
		box["m4"] = map[string]any{"text": "four"}
		box["m1"]["text"] = "one-edited-3"
		b, _ := json.Marshal(box)
		return map[string]json.RawMessage{"inbox": b, "zz": json.RawMessage(`{"a":{"b":1}}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.ReadForm(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inbox=%s zz=%s", f.Keys["inbox"], f.Keys["zz"])
	for k := range f.Keys {
		if len(k) > 0 && (k == "inbox.m4" || k == "zz.a" || k == "zz.a.b") {
			t.Fatalf("flat decoy key %q", k)
		}
	}
	var box map[string]map[string]any
	_ = json.Unmarshal(f.Keys["inbox"], &box)
	if box["m2"]["text"] != "two" || box["m3"]["text"] != "three" || box["m4"]["text"] != "four" || box["m1"]["text"] != "one-edited-3" {
		t.Fatalf("not member-wise: %s", f.Keys["inbox"])
	}
}
