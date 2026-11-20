package keel_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/plugins/keel/keel"
	"github.com/gluck/ebac/sdk/model"
)

// The negative: a keel critic whose FORM carries a token_cmd (a script that leaves a marker) and a keel URL, read
// through the core's critic load, reaches the keel client without either, so a read goes to ebac's door and the
// command never runs; the critic is flagged. RED if Critic() stops applying the trusted config (review F5, #10).
func TestFormTokenCmdIsNeverExecuted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	script := filepath.Join(dir, "tok")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\necho tok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	form.ResetTrusted(filepath.Join(dir, "absent.json"))
	t.Cleanup(func() { form.ResetTrusted(model.TrustedConfigPath) })
	t.Setenv("EBAC_KEEL_SOCKET", filepath.Join(dir, "no-door.sock"))

	critic, _ := json.Marshal(map[string]any{"name": "keel-x", "source": "keel-threads",
		"keel": map[string]any{"repo": "a/b", "url": "https://keel.invalid", "token_cmd": []string{script}}})
	f := &form.CriticForm{ID: "@k", Keys: map[string]json.RawMessage{model.CriticKey: critic}}
	w, err := f.Critic()
	if err != nil {
		t.Fatal(err)
	}
	if w.Keel.URL != "" || len(w.Keel.TokenCmd) != 0 || len(w.FormIgnored) != 2 {
		t.Fatalf("keel %+v ignored %v", w.Keel, w.FormIgnored)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = keel.ForRef(w.Keel).Threads(ctx, "a/b") // the door is absent: the read fails, and must not exec anything
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the form's token_cmd was executed")
	}
	// Control: the same command, given as a TRUSTED value, does run (the instrument can see an execution).
	_, _ = keel.ForRef(&model.KeelRef{Repo: "a/b", URL: "https://127.0.0.1:1", TokenCmd: []string{script}}).Threads(ctx, "a/b")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: a trusted token command did not run: %v", err)
	}
}
