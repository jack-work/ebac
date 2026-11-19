package form

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

// An unreadable config fails closed: defaults only, no per-critic values.
func TestTrustedConfigFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":`), 0o600)
	ResetTrusted(p)
	t.Cleanup(func() { ResetTrusted(model.TrustedConfigPath) })
	if tr := TrustedConfig(); len(tr.Critics) != 0 || tr.ForgeHosts[0] != "microsoft.ghe.com" {
		t.Fatalf("%+v", tr)
	}
}
