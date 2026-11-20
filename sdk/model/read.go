package model

import "time"

// ReadKey is the one key an acknowledgement writes: read.<critic> =
// {round, at, note}. It replaced three flat keys (ack_round, ack_at,
// ack_note), each its own form version and delta to every studier.
const ReadKey = "read"

// LegacyAckKeys are the flat ack keys; an ack removes any it finds, once.
var LegacyAckKeys = []string{"ack_round", "ack_at", "ack_note"}

// Read is one critic's last acknowledgement.
type Read struct {
	Round int    `json:"round"`
	At    string `json:"at"`
	Note  string `json:"note,omitempty"`
}

// ReadMark is the value of the read key after critic acks round at t.
func ReadMark(critic string, round int, t time.Time, note string) map[string]Read {
	return map[string]Read{critic: {Round: round, At: t.UTC().Format(time.RFC3339), Note: FirstLine(note)}}
}
