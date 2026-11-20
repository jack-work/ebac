package poll

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// A missing stamp key (normal before the first ebac review) is reported once per process, not every round; any
// other key failure (a short key) every time. RED if the missing key is logged per round, or a real failure is muted.
func TestMissingStampKeyLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EBAC_STAMP_KEY", filepath.Join(dir, "absent.key"))
	stampKeyMissingLogged.Store(false)
	t.Cleanup(func() { stampKeyMissingLogged.Store(false) })
	var out bytes.Buffer
	p := &Poller{Out: &out}
	w := &model.Critic{Name: "keel-x", FormID: "@f", Keel: &model.KeelRef{Repo: "a/b"}}
	cfg := json.RawMessage(`{"reviewers":["reviewer"]}`)
	for i := 0; i < 3; i++ {
		p.reviewCheck(t.Context(), w, cfg, nil)
	}
	if n := strings.Count(out.String(), "review check: stamp key"); n != 1 {
		t.Fatalf("missing key logged %d times in 3 rounds, want 1:\n%s", n, out.String())
	}
	short := filepath.Join(dir, "short.key")
	_ = os.WriteFile(short, []byte("x"), 0o600)
	t.Setenv("EBAC_STAMP_KEY", short)
	out.Reset()
	for i := 0; i < 2; i++ {
		p.reviewCheck(t.Context(), w, cfg, nil)
	}
	if n := strings.Count(out.String(), "want 32"); n != 2 {
		t.Fatalf("a short key logged %d times in 2 rounds, want 2:\n%s", n, out.String())
	}
}
