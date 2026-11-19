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

// Authority comes from the trusted config, else ebac's creation record, else nowhere (not run, least authority).
// The creation record is written once, on the save that creates the critic, and a form edit never changes what is
// read back. RED if a form write/approve/source reaches the critic, or a critic with no authority keeps its grants.
func TestAuthorityComesFromConfigOrCreationRecord(t *testing.T) {
	dir := t.TempDir()
	ResetTrusted(filepath.Join(dir, "absent.json"))
	t.Cleanup(func() { ResetTrusted(model.TrustedConfigPath) })
	old := authorityDir
	authorityDir = filepath.Join(dir, "authority")
	t.Cleanup(func() { authorityDir = old })
	critic := func(m map[string]any) *CriticForm {
		b, _ := json.Marshal(m)
		return &CriticForm{ID: "@f1", Keys: map[string]json.RawMessage{model.CriticKey: b}}
	}
	seat := map[string]any{"name": "x", "source": "org-approve", "mode": "reviewer", "write": true, "approve": true,
		"org_approve": map[string]any{"grant": "g", "principal": "p", "approver": "eve"}}

	// No config, no record: not run, least authority.
	w, err := critic(seat).Critic()
	if err != nil {
		t.Fatal(err)
	}
	if !w.NoAuthority || w.Write || w.Approve || w.OrgApprove.Approver != "" || w.OrgApprove.Grant != "" || w.Source != "" || w.Mode != "" {
		t.Fatalf("no authority: %+v %+v", w, w.OrgApprove)
	}
	// A creation record (as SaveRound writes it for a new critic) is what is read back, not the form.
	s := &Store{}
	if err := s.RegisterAuthority(&model.Critic{FormID: "@f1", Source: "teams-chat", Mode: model.ModeTeams, Write: true, Approve: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAuthority(&model.Critic{FormID: "@f1", Source: "org-approve", Write: true}); err == nil {
		t.Fatal("a second record overwrote the first")
	}
	w, _ = critic(seat).Critic()
	if w.NoAuthority || w.Source != "teams-chat" || w.Mode != model.ModeTeams || w.Write || w.Approve {
		t.Fatalf("record: %+v", w)
	}
	if !strings.Contains(strings.Join(w.FormIgnored, ","), "source,mode,write,approve") {
		t.Fatalf("not flagged: %v", w.FormIgnored)
	}
	// The trusted config wins over the record (positive control: a granted write IS read).
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":{"critics":{"@f1":{"authority":{"source":"keel-threads","mode":"keel","write":true}}}}}`), 0o600)
	ResetTrusted(p)
	w, _ = critic(seat).Critic()
	if w.Source != "keel-threads" || !w.Write || w.Approve {
		t.Fatalf("config: %+v", w)
	}
}

// adoptWritten re-applies the authority: a seat's mid-round write=true in the written key does not reach the
// critic (review f0f3a1d9 M1). RED if adoptWritten stops overlaying authority.
func TestAdoptWrittenReappliesAuthority(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":{"critics":{"@f2":{"authority":{"source":"keel-threads","mode":"keel"}}}}}`), 0o600)
	ResetTrusted(p)
	t.Cleanup(func() { ResetTrusted(model.TrustedConfigPath) })
	w := &model.Critic{Name: "k", FormID: "@f2", Source: "keel-threads", Mode: model.ModeKeel}
	seat, _ := json.Marshal(map[string]any{"name": "k", "source": "org-approve", "mode": "reviewer", "write": true})
	adoptWritten(w, seat)
	if w.Write || w.Source != "keel-threads" || w.Mode != model.ModeKeel {
		t.Fatalf("adopted %+v", w)
	}
}
