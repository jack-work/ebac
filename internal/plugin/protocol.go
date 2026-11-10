// Package plugin is the one protocol between the ebac core and its source
// plugins: separate executables (ebac-gh, ebac-keel, ebac-tomb, ...) that
// translate one upstream each.
//
// One exec per operation: the core writes ONE JSON Request on the plugin's
// stdin and reads ONE JSON Reply from its stdout; the plugin's stderr goes to
// the core's stderr (the journal). A plugin that exits non-zero, times out,
// writes more than MaxReply bytes, or writes anything that is not a Reply of
// this Version fails the round with that error; it never crashes the core.
//
// Division of labour. A plugin reads and writes its upstream and nothing
// else: it fetches, diffs, and returns events, inbox items, form keys, gone
// markers and its own state. The core owns everything figaro: the role form
// (one Commit per round), the trigger rules, pending events, delivery, the
// lease, succession, archival and timers. Plugins run as the core's uid, so
// the boundary is the sandbox (Sandbox): each plugin execs under bubblewrap
// with the angelus socket's directory hidden from its mount namespace.
package plugin

import (
	"encoding/json"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
)

// Version is the protocol revision; a reply of any other version is refused.
const Version = 1

// MaxReply caps a plugin's stdout. A reply this large is a bug, not news.
const MaxReply = 8 << 20

type Op string

const (
	// OpRound fetches the upstream once and returns what changed.
	OpRound Op = "round"
	// OpPost takes posts the core has already AUTHORIZED as far as they go
	// upstream and returns them with their receipt fields set.
	OpPost Op = "post"
	// OpBrief renders the wake message for the pending events.
	OpBrief Op = "brief"
)

// Event is a model.Event on the wire. model.Event keeps its full text out of
// every persisted form (json:"-"); the trigger rules match on that text, so
// the wire carries it explicitly.
type Event struct {
	model.Event
	Text string `json:"text,omitempty"`
	// Inbox names the inbox items this event produced, so the core can drop
	// them with the event when a trigger rule says drop.
	Inbox []string `json:"inbox,omitempty"`
}

func Wire(evs []model.Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = Event{Event: e, Text: e.Text}
	}
	return out
}

func Unwire(evs []Event) []model.Event {
	out := make([]model.Event, len(evs))
	for i, e := range evs {
		out[i] = e.Event
		out[i].Text = e.Text
	}
	return out
}

// Request is everything a plugin may know. It is the only input: a plugin
// keeps no state of its own between execs (its state rides in the critic's
// runtime and comes back in the Reply).
type Request struct {
	Protocol int          `json:"protocol"`
	Op       Op           `json:"op"`
	Critic   model.Critic `json:"critic"`
	// Snapshot is the previous round's snapshot on OpRound, and the current
	// one on OpPost and OpBrief.
	Snapshot *model.Snapshot `json:"snapshot,omitempty"`
	// Seat is the aria holding the critic's alert target (keel names it).
	Seat string `json:"seat,omitempty"`
	// Me is the operator's upstream login, when the core already knows it.
	Me     string `json:"me,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// GH and Host are the core's --gh and --host flags.
	GH   string `json:"gh,omitempty"`
	Host string `json:"host,omitempty"`

	// Keys are the role-form keys the source reads (model.SourceReads): its
	// holder-edited configuration, read fresh every round.
	Keys map[string]json.RawMessage `json:"keys,omitempty"`

	// Inputs are the keys of the critic's inputs form (the scout: merge rule,
	// ledger acks), read by the core each round.
	Inputs map[string]json.RawMessage `json:"inputs,omitempty"`
	// Board is the board form's owner marks (keys prs, items), read-only,
	// read by the core each round (the scout's merge checklist).
	Board map[string]json.RawMessage `json:"board,omitempty"`

	// Arias is the aria store, read by the core (roster plugin only).
	Arias []model.Aria `json:"arias,omitempty"`

	Posts   map[string]*model.Post `json:"posts,omitempty"`   // OpPost
	Pending []Event                `json:"pending,omitempty"` // OpBrief
	Retry   int                    `json:"retry,omitempty"`   // OpBrief
}

// Ping is a one-line message the core sends to a role (the scout: a material
// issue change to the owner role of each PR linking it).
type Ping struct {
	To   string `json:"to"` // a role form, @<id>
	Text string `json:"text"`
}

// FormWrite is one write to the critic's inputs form (the scout's ledger
// outbox): Set's ledger_outbox members are merged into the form's, and a null
// member is removed. Inputs names the target and must be set.
type FormWrite struct {
	Inputs bool                       `json:"inputs,omitempty"`
	Set    map[string]json.RawMessage `json:"set,omitempty"`
}

// Gone is one resource the plugin has given up on (form key gone.<Key>).
type Gone struct {
	Key      string `json:"key"`
	Resource string `json:"resource"`
	Since    string `json:"since"`
	Error    string `json:"error"`
	Action   string `json:"action"`
}

type Reply struct {
	Protocol int `json:"protocol"`

	// Critic is the critic as the plugin leaves it. The core adopts only the
	// fields a source owns (see Adopt); everything else stays the core's.
	Critic *model.Critic `json:"critic,omitempty"`
	// Snapshot, when set, is saved as the critic's snapshot.
	Snapshot *model.Snapshot           `json:"snapshot,omitempty"`
	Events   []Event                   `json:"events,omitempty"`
	Inbox    map[string]form.InboxItem `json:"inbox,omitempty"`
	// Set and Unset are role-form keys the source owns (threads, authored,
	// blind, ...). The core writes a key only when its value changed.
	Set   map[string]json.RawMessage `json:"set,omitempty"`
	Unset []string                   `json:"unset,omitempty"`
	Gone  []Gone                     `json:"gone,omitempty"`
	// Pings are one-line messages to roles (the scout only; ValidatePings).
	Pings []Ping `json:"pings,omitempty"`
	// Forms are writes to the critic's inputs form (the scout's ledger
	// outbox), validated whole (ValidateForms) before any of the reply is
	// applied.
	Forms []FormWrite `json:"forms,omitempty"`
	// Wake are one-off messages for the alert target (blind, gone).
	Wake []string `json:"wake,omitempty"`
	// DisableTimer stops the critic's timer (the source stopped it for good).
	DisableTimer bool `json:"disable_timer,omitempty"`
	// Me is the operator's upstream login, if the plugin resolved it.
	Me string `json:"me,omitempty"`
	// Done ends the round early: effects are applied, nothing is delivered.
	Done bool `json:"done,omitempty"`
	// Error fails the round after its effects are applied.
	Error string `json:"error,omitempty"`
	// Archive asks the core to run its archival check (PR sources).
	Archive bool `json:"archive,omitempty"`
	// Succession asks the core to supervise the seat's succession after a
	// delivered round.
	Succession bool `json:"succession,omitempty"`
	// Outbox says this source carries posts: the core flushes the outbox
	// through OpPost after the round.
	Outbox bool `json:"outbox,omitempty"`

	Posts map[string]*model.Post `json:"posts,omitempty"` // OpPost
	Brief string                 `json:"brief,omitempty"` // OpBrief

	// Log lines go to the core's output, prefixed by nothing.
	Log []string `json:"log,omitempty"`
}

// Logf appends one line to the reply's log.
func (r *Reply) Logf(format string, args ...any) {
	r.Log = append(r.Log, sprintf(format, args...))
}

// SetKey sets a role-form key to v.
func (r *Reply) SetKey(key string, v any) {
	if r.Set == nil {
		r.Set = map[string]json.RawMessage{}
	}
	r.Set[key] = model.MustJSON(v)
}

// Adopt copies the fields a source owns from the plugin's critic onto the
// core's. Identity, mode, grants, delivery, triggers, succession, the outbox
// watermark and archival stay the core's whatever the plugin returns.
func Adopt(core, from *model.Critic) {
	if from == nil {
		return
	}
	core.PRs = from.PRs
	r, f := &core.Runtime, &from.Runtime
	r.LastSuccess = f.LastSuccess
	r.LastError = f.LastError
	r.ConsecutiveFailures = f.ConsecutiveFailures
	r.GoneStreak = f.GoneStreak
	r.Teams = f.Teams
	r.Author = f.Author
	r.Keel = f.Keel
	r.BandHub = f.BandHub
	r.Scout = f.Scout
	r.Roster = f.Roster
	r.Releases = f.Releases
	r.Stopped = f.Stopped
	r.StopReason = f.StopReason
}
