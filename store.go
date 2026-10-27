package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ---------- paths ----------

// Store holds ebac's state. Critic state lives in figaro forms (formstate.go);
// Root keeps only what must outlive a form: archive exports and reports.
type Store struct {
	Root   string
	Fig    *Figaro
	Me     string            // the forge login, for protocol text
	formOf map[string]string // critic name -> form id, for this process
}

func NewStore(fig *Figaro) (*Store, error) {
	root := os.Getenv("EBAC_STATE")
	if root == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			base = filepath.Join(home, ".local", "state")
		}
		root = filepath.Join(base, "ebac")
	}
	for _, d := range []string{"archive", "reports"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root, Fig: fig, formOf: map[string]string{}}, nil
}

// writeJSONAtomic writes via a temp file and rename. A critic definition
// truncated by a crash mid-write is a critic that silently stops criticing.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
