package form

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fform "github.com/jack-work/figaro/api/form"
)

// The 2026-11-10 wipe, at its root: ebac computes every write as the diff of the board it reads and the board it
// wants, and figaro's form library before v0.51.8 (f21d0cc6) rebuilt a deleted key's in-order successor without its
// subtree. On a Teams critic's board, removing blind nulled critic (and removing target-aria nulled trigger). Every
// other key must survive the removal of any one key, on that board's exact key set.
func TestRemovingAKeyKeepsEveryOtherKey(t *testing.T) {
	keys := []string{"ack_at", "ack_note", "ack_round", "blind", "critic", "harness", "harness_protocol_1", "inbox", "kind",
		"name", "system", "target-aria", "trigger"}
	for _, del := range keys {
		m := map[string]json.RawMessage{}
		for _, k := range keys {
			m[k] = json.RawMessage(`{"a":1,"b":{"c":2}}`)
		}
		raw, _ := json.Marshal(m)
		var snap fform.Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatal(err)
		}
		next, err := nextBoard(snap, nil, []string{del})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(next)
		var got map[string]json.RawMessage
		_ = json.Unmarshal(b, &got)
		for _, k := range keys {
			if k == del {
				if got[k] != nil {
					t.Errorf("removing %s left it: %s", del, got[k])
				}
				continue
			}
			if string(got[k]) != string(m[k]) {
				t.Errorf("removing %s changed %s: %s -> %s", del, k, m[k], got[k])
			}
		}
	}
}

const goodCritic = `{"name":"teams-x","mode":"teams","source":"teams-chat","chat":{"id":"19:a@thread.v2"},"stop":{}}`

func board(critic string) []byte {
	m := map[string]json.RawMessage{"kind": json.RawMessage(`"ebac.critic"`)}
	if critic != "" {
		m["critic"] = json.RawMessage(critic)
	}
	b, _ := json.Marshal(m)
	return b
}

// The guard at the write: a usable critic key may only be replaced by a usable one naming the same critic. A board
// without a usable critic is unprotected, so a repair may land.
func TestGuardCritic(t *testing.T) {
	for _, bad := range []string{`{}`, `null`, `{"mode":"teams"}`, `"x"`, ``, `{"name":"other"}`} {
		err := guardCritic(json.RawMessage(goodCritic), board(bad))
		if !errors.Is(err, ErrCriticGuard) {
			t.Errorf("next critic %q: not refused (%v)", bad, err)
		}
	}
	if err := guardCritic(json.RawMessage(goodCritic), board(goodCritic)); err != nil {
		t.Errorf("an unchanged critic refused: %v", err)
	}
	if err := guardCritic(json.RawMessage(goodCritic), board(strings.Replace(goodCritic, `"stop":{}`, `"stop":{},"ping_every_sec":1800`, 1))); err != nil {
		t.Errorf("an edited critic refused: %v", err)
	}
	if err := guardCritic(json.RawMessage(`{}`), board(goodCritic)); err != nil {
		t.Errorf("a repair refused: %v", err)
	}
}

// An unusable critic on the form is an error for the merge, never a set of deletions: merged as one, {} on the form
// "removed" every field and the merge wrote critic: {}.
func TestMergeRefusesAnUnusableFormCritic(t *testing.T) {
	for _, theirs := range []string{`{}`, `null`, `{"mode":"teams"}`} {
		got, _, err := mergeCritic(json.RawMessage(goodCritic), json.RawMessage(goodCritic), json.RawMessage(theirs))
		if err == nil {
			t.Errorf("theirs %s: merged to %s", theirs, got)
		}
	}
	if got, _, err := mergeCritic(json.RawMessage(goodCritic), json.RawMessage(goodCritic), json.RawMessage(goodCritic)); err != nil || !strings.Contains(string(got), `"name":"teams-x"`) {
		t.Fatalf("control: %s %v", got, err)
	}
}
