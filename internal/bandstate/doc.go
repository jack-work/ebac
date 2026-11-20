// Package bandstate mirrors the band hub into one studied role form: queues,
// node capacity, live jobs and duplicate or superseded work, at a glance. It
// is the logic of the ebac-bandhub plugin: Fetch reads the hub, Step turns a
// read and the last state into the form keys to write. It never touches
// figaro (plugins never dial it; the core writes the keys).
//
// It reads only the hub's metadata (GET /stats, /nodes, /jobs?state=..., the
// hub's metaReaders view) and decodes only the fields it renders: never job
// logs, outputs, artifacts, diagnostics or reasons, which can quote a log
// (cheroot 2026-11-09). Every job field outside the meta allowlist (ids,
// states, times, counts, payload template + branch/sha/label/node) is
// optional here: absent renders as "-".
//
// It never wakes anyone. Studiers see the change in their study diff, so the
// whole design is about writing a key ONLY when its value changes: ages are
// rendered as instants (since=HH:MMZ), never as ages, and node staleness shows
// only past the hub's own threshold.
package bandstate
