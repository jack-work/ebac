package model

import (
	"encoding/json"
	"strings"
)

func MustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// objectOf reads a JSON object's members; anything else reads as empty.
func ObjectOf(raw json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

// literalJSON encodes without HTML escaping, so a body's <!-- marker --> is
// sent as written.
func LiteralJSON(v any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return []byte(strings.TrimSpace(b.String()))
}
