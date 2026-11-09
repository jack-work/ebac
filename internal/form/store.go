package form

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/gluck/ebac/internal/figaro"
)

// ---------- paths ----------

// Store holds ebac's state. Critic state lives in figaro forms (formstate.go);
// Root keeps only what must outlive a form: archive exports and reports.
type Store struct {
	Root   string
	Fig    *figaro.Figaro
	Me     string            // the forge login, for protocol text
	formOf map[string]string // critic name -> form id, for this process
	// loaded is each critic key as this process last read or wrote it, by
	// form id: the base of SaveRound's merge (critic_merge.go).
	loaded map[string]json.RawMessage
	// Warn receives merge conflicts; nil is os.Stderr.
	Warn io.Writer
}

func NewStore(fig *figaro.Figaro) (*Store, error) {
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

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// remember records a critic key as this process saw it: the next save merges
// against it.
func (s *Store) remember(id string, raw json.RawMessage) {
	if id == "" || len(raw) == 0 {
		return
	}
	if s.loaded == nil {
		s.loaded = map[string]json.RawMessage{}
	}
	s.loaded[id] = append(json.RawMessage(nil), raw...)
}
