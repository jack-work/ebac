package form

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// The save path: what is written to the form carries no trusted value, and the caller's critic keeps them; the
// written key adopted back is overlaid again, so a seat's mid-round notes never reach the brief. RED if criticConfig
// stops stripping or adoptWritten stops re-applying (reviews e025e175 M2, 5fd38 #4).
func TestSaveNeverWritesTrustedAndAdoptReapplies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":{"critics":{"@f":{"notes":"TRUSTED.md","keel_url":"https://k","token_cmd":["hush"]}}}}`), 0o600)
	ResetTrusted(p)
	t.Cleanup(func() { ResetTrusted(model.TrustedConfigPath) })
	w := &model.Critic{Name: "keel-x", FormID: "@f", Source: "keel-threads", Notes: "TRUSTED.md", Keel: &model.KeelRef{URL: "https://k", TokenCmd: []string{"hush"}}}
	m := criticConfig(w)
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "TRUSTED.md") || strings.Contains(string(b), "https://k") || strings.Contains(string(b), "hush") {
		t.Fatalf("written critic key carries trusted values: %s", b)
	}
	if w.Notes != "TRUSTED.md" || w.Keel.URL != "https://k" {
		t.Fatalf("the caller's critic lost its trusted values: %+v", w)
	}
	seat, _ := json.Marshal(map[string]any{"name": "keel-x", "source": "keel-threads", "notes": "/etc/shadow", "keel": map[string]any{"repo": "a/b"}})
	adoptWritten(w, seat)
	if w.Notes != "TRUSTED.md" || w.Keel.URL != "https://k" || strings.Join(w.FormIgnored, ",") != "notes" {
		t.Fatalf("adopted: notes %q keel %+v ignored %v", w.Notes, w.Keel, w.FormIgnored)
	}
}
