package form

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/gluck/ebac/internal/figaro"
)

// ---------- paths ----------

// Store holds ebac's state. Critic state lives in figaro forms (formstate.go);
// Root keeps only what must outlive a form: archive exports and reports.
type Store struct {
	Root string
	Fig  *figaro.Figaro
	Me   string // the forge login, for protocol text
	// mu guards formOf and loaded: ebacd's scheduler runs several critics'
	// rounds at once on one Store (a concurrent map write killed ebacd,
	// 2026-11-19 22:54Z). Touch them only through the helpers below.
	mu     sync.Mutex
	formOf map[string]string // critic name -> form id, for this process
	// loaded is each critic key as this process last read or wrote it, by
	// form id: the base of SaveRound's merge (critic_merge.go).
	loaded map[string]json.RawMessage
	// Warn receives merge conflicts; nil is os.Stderr.
	Warn io.Writer
}

// StateRoot is ebac's state directory: $EBAC_STATE, else
// $XDG_STATE_HOME/ebac, else ~/.local/state/ebac.
func StateRoot() (string, error) {
	if root := os.Getenv("EBAC_STATE"); root != "" {
		return root, nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ebac"), nil
}

func NewStore(fig *figaro.Figaro) (*Store, error) {
	root, err := StateRoot()
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"archive", "reports"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	if authorityDir == "" {
		authorityDir = filepath.Join(root, "authority")
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded == nil {
		s.loaded = map[string]json.RawMessage{}
	}
	s.loaded[id] = append(json.RawMessage(nil), raw...)
}

// base is the critic key this process last saw for a form (nil if none).
func (s *Store) base(id string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded[id]
}

// knownForm is the form id this process last resolved for a critic name.
func (s *Store) knownForm(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.formOf[name]
	return id, ok
}

// setForm records (id != "") or forgets (id == "") a critic name's form.
func (s *Store) setForm(name, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		delete(s.formOf, name)
		return
	}
	if s.formOf == nil {
		s.formOf = map[string]string{}
	}
	s.formOf[name] = id
}
