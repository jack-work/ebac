package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Mode string

const (
	ModeReview   Mode = "review"   // critic, report, stay quiet
	ModeDevelop  Mode = "develop"  // also audit the harness and improve it
	ModeReviewer Mode = "reviewer" // review someone else's PRs: comment, reply, maybe approve
)

type StopConditions struct {
	// UntilAllClosed archives the critic once every PR is closed or merged
	// and the grace period has elapsed.
	UntilAllClosed bool `json:"until_all_closed"`
	// ArchiveGraceSec keeps polling after the last PR closes, so a
	// post-merge comment still reaches the seat.
	ArchiveGraceSec int `json:"archive_grace_sec"`
	// MaxRounds and Deadline are hard stops; 0 / zero-time mean unbounded.
	MaxRounds int        `json:"max_rounds,omitempty"`
	Deadline  *time.Time `json:"deadline,omitempty"`
	// MaxConsecutiveFailures stops a critic that cannot reach GitHub, so a
	// dead credential does not generate an infinite error log.
	MaxConsecutiveFailures int `json:"max_consecutive_failures"`
}

func DefaultStop() StopConditions {
	return StopConditions{
		UntilAllClosed:         true,
		ArchiveGraceSec:        6 * 3600,
		MaxConsecutiveFailures: 20,
	}
}

type Runtime struct {
	Round               int       `json:"round"`
	LastPolled          time.Time `json:"last_polled"`
	LastSuccess         time.Time `json:"last_success"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`

	// PendingEvents survive a failed delivery. Persisting the debt BEFORE
	// invoking figaro is what makes a crash mid-alert recoverable rather
	// than silent; clearing it only on a confirmed send is what stops the
	// same news being delivered twice.
	PendingEvents  []Event   `json:"pending_events,omitempty"`
	PendingSince   time.Time `json:"pending_since,omitempty"`
	RetryCount     int       `json:"retry_count,omitempty"`
	NextRetryAt    time.Time `json:"next_retry_at,omitempty"`
	LastDelivered  time.Time `json:"last_delivered,omitempty"`
	DeliveredRound int       `json:"delivered_round,omitempty"`

	AllClosedSince *time.Time `json:"all_closed_since,omitempty"`

	// Succession bookkeeping. Generation counts completed hand-overs, so a
	// critic can say how many seats it has burned through.
	Generation      int    `json:"generation,omitempty"`
	Standby         string `json:"standby,omitempty"`
	StandbyMintedAt string `json:"standby_minted_at,omitempty"`
	LastRecast      string `json:"last_recast,omitempty"`

	// OutboxWatermark is the form version up to which every post has
	// reached an end (outbox.go).
	OutboxWatermark uint64 `json:"outbox_watermark,omitempty"`

	// GoneStreak counts consecutive "not found" rounds per PR key.
	GoneStreak map[string]int `json:"gone_streak,omitempty"`

	// Teams is a teams critic's seen-map and gone tracking.
	Teams *TeamsState `json:"teams,omitempty"`

	Stopped    bool      `json:"stopped"`
	StopReason string    `json:"stop_reason,omitempty"`
	Archived   bool      `json:"archived"`
	ArchivedAt time.Time `json:"archived_at,omitempty"`
}

type Critic struct {
	Name      string    `json:"name"`
	FormID    string    `json:"form_id"`
	Mode      Mode      `json:"mode"`
	Host      string    `json:"host"`
	CreatedAt time.Time `json:"created_at"`
	// EverySec is the critic's poll interval as armed at creation. Reconcile
	// re-arms a stopped timer with it; 0 means the default (5 minutes).
	EverySec int `json:"every_sec,omitempty"`

	PRs []PRRef `json:"prs"`

	// Discovery keeps a critic's PR set live: any PR matching the filter
	// joins the critic on its next round, and one that leaves the filter is
	// retained until it closes (a PR does not stop mattering because a
	// label moved).
	DiscoverRepo   string `json:"discover_repo,omitempty"`
	DiscoverAuthor string `json:"discover_author,omitempty"`

	// IssueRepo is where a develop-mode seat files harness defects.
	IssueRepo string `json:"issue_repo,omitempty"`

	// Write grants the seat authority to ACT on this critic's pull requests —
	// reply to threads, comment, push to the head branch. Off by default and
	// per-critic on purpose: the read-only default exists because an agent with
	// GitHub credentials and a vague charge will eventually decide that
	// commenting would be helpful, and a global flip would silently authorize
	// every critic in the fleet. Turning it on is the operator saying "this
	// seat, these PRs". It never grants approve/merge/close — those stay out of
	// reach in both modes, because they are irreversible and an agent that can
	// merge its own work has no reviewer.
	Write bool `json:"write,omitempty"`

	// Approve lets a reviewer seat approve PRs it did not author. Never
	// request-changes, merge or close.
	Approve bool `json:"approve,omitempty"`

	// Worktree, when set, is where a reviewer checks out each PR head:
	// <Root>/<critic>-pr<N>, fetched through Clone.
	Worktree *Worktree `json:"worktree,omitempty"`

	// Notes names repo-specific review guidance the seat reads first.
	Notes string `json:"notes,omitempty"`

	FigaroSock string `json:"figaro_socket,omitempty"`

	// Source names the upstream this critic translates (source.go); empty
	// means github-pr.
	Source string `json:"source,omitempty"`

	// Chat is the Teams chat a teams critic watches (Mode teams only).
	Chat *ChatRef `json:"chat,omitempty"`

	Stop    StopConditions `json:"stop"`
	Runtime Runtime        `json:"runtime"`
}

type Worktree struct {
	Clone string `json:"clone"`
	Root  string `json:"root"`
}

func (w *Critic) prRefs() map[PRKey]PRRef {
	m := map[PRKey]PRRef{}
	for _, r := range w.PRs {
		m[r.Key()] = r
	}
	return m
}

func (w *Critic) AddPR(r PRRef) bool {
	if _, ok := w.prRefs()[r.Key()]; ok {
		return false
	}
	w.PRs = append(w.PRs, r)
	sort.Slice(w.PRs, func(i, j int) bool { return w.PRs[i].Key() < w.PRs[j].Key() })
	return true
}

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

func ValidName(n string) error {
	if n == "" {
		return fmt.Errorf("critic name is required")
	}
	if len(n) > 64 {
		return fmt.Errorf("critic name too long")
	}
	for _, r := range n {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("critic name %q: use only [A-Za-z0-9_-] (it becomes a systemd instance name)", n)
		}
	}
	return nil
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

// ValidAriaID rejects the shapes that reach this tool by accident.
//
// `figaro bind -j` emits {figaro_id, form_id} while `figaro new -j` emits
// {aria_id}; a caller reading the wrong field gets the literal string "null"
// and casting it fails deep inside the WAL with an unhelpful message. An
// @-sigiled id is a FORM, not an aria, and a role may not target another
// form — roles do not chain.
func ValidAriaID(id string) error {
	if id == "" {
		return fmt.Errorf("aria id is required")
	}
	if id == "null" || id == "<nil>" || id == "undefined" {
		return fmt.Errorf("aria id %q looks like a JSON field that was missing: `figaro bind -j` returns figaro_id, `figaro new -j` returns aria_id", id)
	}
	if strings.HasPrefix(id, "@") {
		return fmt.Errorf("%q is a form, not an aria: a role's target-aria must name a figaro (roles do not chain)", id)
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return fmt.Errorf("aria id %q is not hex", id)
		}
	}
	return nil
}

// Interval is the critic's poll interval: EverySec, else the default 5 minutes.
func (w *Critic) Interval() time.Duration {
	if w.EverySec > 0 {
		return time.Duration(w.EverySec) * time.Second
	}
	return 5 * time.Minute
}
