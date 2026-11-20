package form

import (
	"encoding/json"

	"github.com/gluck/ebac/sdk/model"
)

// Hooks for form_test, which also imports the github source (a cycle for an
// in-package test).

// LegacyPRsForTest is prs as ebac wrote it before heads: the whole tree.
func LegacyPRsForTest(snap *model.Snapshot) json.RawMessage { return model.MustJSON(prsTree(snap)) }

// SaveRoundPRsForTest is SaveRound's prs half without a daemon: the heads it
// sets on keys, then the detail file it writes after the commit.
func SaveRoundPRsForTest(s *Store, id string, keys map[string]json.RawMessage, snap *model.Snapshot) error {
	heads, detail := roundPRs(snap)
	keys["prs"] = heads
	keys["system"] = model.MustJSON(map[string]any{"ebac": EbacState{Round: snap.Round, TakenAt: snap.TakenAt, Complete: snap.Complete}})
	return s.writePRsLocal(id, detail)
}

// ReadFormForTest is ReadForm on keys already in hand: the detail file is
// read as ReadForm reads it.
func ReadFormForTest(s *Store, id string, keys map[string]json.RawMessage) *CriticForm {
	return &CriticForm{ID: id, Keys: keys, prsLocal: s.readPRsLocal(id)}
}

func PRsPathForTest(s *Store, id string) string { return s.prsPath(id) }
