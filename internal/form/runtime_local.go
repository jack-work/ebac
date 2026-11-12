package form

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// A scout's runtime state (Runtime.Scout: the cursor, every tracked PR's and
// issue's head and entry digest, ledger outbox marks) is the core's to keep,
// and it is large: ~47 KB on the effort-github form on 2026-11-12, a third of
// the form, read by nobody but the scout itself. It lives on disk beside the
// store, never on the form:
//
//   - ReadForm reads <Root>/runtime/<form>.scout.json into CriticForm.local,
//     and ebac() overlays Runtime.Scout from it. Keys and Snap stay the form's,
//     so Commit still diffs the real form.
//   - Commit strips system.ebac.runtime.scout from every write, whoever builds
//     it, so no path can put the state back on a form.
//   - SaveRound writes the file after its commit lands, and only when the
//     state is non-empty: a critic loaded without its file never wipes it.
//
// An absent file falls back to the form's copy (the one-time migration: the
// first save moves it); an absent or unreadable file with no form copy is an
// empty state, which the scout reads as a from-nothing pass. The file is
// written after the form, so it is never AHEAD of the entries the form holds:
// a crash in between leaves it behind, and a state that is behind only makes
// the scout rewrite an entry it already wrote.

const localDir = "runtime"

func (s *Store) localPath(formID string) string {
	return filepath.Join(s.Root, localDir, strings.TrimPrefix(formID, "@")+".scout.json")
}

// readLocal returns the form's scout state from disk, or nil when there is
// none or it is unreadable (said on Warn: the round then starts from the
// form's copy, or from nothing).
func (s *Store) readLocal(formID string) json.RawMessage {
	b, err := os.ReadFile(s.localPath(formID))
	if err != nil {
		if !os.IsNotExist(err) {
			s.warnf("ebac: scout state %s unreadable, starting from the form's copy: %v\n", s.localPath(formID), err)
		}
		return nil
	}
	if !json.Valid(b) || len(strings.TrimSpace(string(b))) == 0 {
		s.warnf("ebac: scout state %s is not JSON, starting from the form's copy\n", s.localPath(formID))
		return nil
	}
	return json.RawMessage(b)
}

// writeLocal replaces the form's scout state on disk atomically, under a
// unique temp name, so two writers never share a temp file. Empty state is
// never written.
func (s *Store) writeLocal(formID string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	path := s.localPath(formID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Store) warnf(format string, a ...any) {
	var w io.Writer = os.Stderr
	if s.Warn != nil {
		w = s.Warn
	}
	fmt.Fprintf(w, format, a...)
}

// stripScout is stripLocal; a test swaps it to write a form as ebac did
// before the state moved to disk.
var stripScout = stripLocal

// stripLocal removes system.ebac.runtime.scout from a write's set. It touches
// nothing when the set does not carry that member, and never adds a member.
func stripLocal(set map[string]json.RawMessage) {
	raw, ok := set["system"]
	if !ok {
		return
	}
	system := model.ObjectOf(raw)
	ebacRaw, ok := system["ebac"]
	if !ok {
		return
	}
	e := model.ObjectOf(ebacRaw)
	rtRaw, ok := e["runtime"]
	if !ok {
		return
	}
	rt := model.ObjectOf(rtRaw)
	if _, ok := rt["scout"]; !ok {
		return
	}
	delete(rt, "scout")
	e["runtime"] = model.MustJSON(rt)
	system["ebac"] = model.MustJSON(e)
	set["system"] = model.MustJSON(system)
}
