package model

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
	// MaxRounds is a hard stop; 0 means unbounded.
	MaxRounds int `json:"max_rounds,omitempty"`
	// Deadline ENDS the critic at that moment (ebac set --end until=...).
	Deadline *time.Time `json:"deadline,omitempty"`
	// ExpiresAfterSec ENDS the critic this long after RenewedAt (else
	// CreatedAt) unless its owner renews it (ebac set --renew); 0 = never.
	ExpiresAfterSec int        `json:"expires_after_sec,omitempty"`
	RenewedAt       *time.Time `json:"renewed_at,omitempty"`
	// WithRole is a STANDING end: the critic lives while this role form is in
	// the roster and ends when the role is retired (its form gone) or absorbed
	// (moved-to set). No calendar expiry; only for critics on a roster role.
	WithRole string `json:"with_role,omitempty"`
	// MaxConsecutiveFailures stops a critic that cannot reach GitHub, so a
	// dead credential does not generate an infinite error log.
	MaxConsecutiveFailures int `json:"max_consecutive_failures"`
}

// DefaultStop is the TRANSIENT default for a critic created without --end
// (reviews, PR and thread watches; the Boss 11-19): it ends when every PR is
// closed plus 6 h, and in any case 7 days after creation unless renewed.
func DefaultStop() StopConditions {
	return StopConditions{
		UntilAllClosed:         true,
		ArchiveGraceSec:        6 * 3600,
		ExpiresAfterSec:        7 * 86400,
		MaxConsecutiveFailures: 20,
	}
}

type Runtime struct {
	// EndedAt: ebacd ended the critic (archived; its forms are killed, the
	// kills retried until the role form is gone). EndWarnedFor is the expiry
	// an advance warning was sent for (a renewal moves the expiry, so a new
	// warning is due).
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EndWarnedFor *time.Time `json:"end_warned_for,omitempty"`
	// PurgeAt is when an archived critic whose forms still stand (archived by
	// hand, or before ends existed) is purged: its forms killed. nil = its
	// ArchivedAt + DefaultPurgeAfter.
	PurgeAt *time.Time `json:"purge_at,omitempty"`

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

	// TriggerGood is the last rule set that parsed: what stays in force while
	// the form's trigger key is broken. TriggerErr is the error last written
	// to the form's trigger_error key ("" when that key is absent).
	TriggerGood json.RawMessage `json:"trigger_good,omitempty"`
	TriggerErr  string          `json:"trigger_err,omitempty"`

	// Releases is a release watch's state: the tags already seen.
	Releases *ReleasesState `json:"releases,omitempty"`
	// ApprovePosts are approve-requests' post ledger, by request id: the
	// approve each request prepared, and whether it was posted (#27 B).
	ApprovePosts map[string]*PostRecord `json:"approve_posts,omitempty"`
	// OrgApprove is an org-approve critic's scores and filed asks.
	OrgApprove *OrgApproveState `json:"org_approve,omitempty"`
	// LastPing is when a collect-mode critic last pinged its seat.
	LastPing time.Time `json:"last_ping,omitempty"`

	// Children are the child forms the core keeps for the critic (keel
	// branches: one per work branch), by the plugin's stable key: key -> @id.
	// Core-owned.
	Children map[string]string `json:"children,omitempty"`
	// FormsLanded is how many of the last reply's form writes landed, in
	// order (landForms stops at the first failure). Core-owned: a plugin
	// compares it with what it sent and writes the rest again.
	FormsLanded int `json:"forms_landed,omitempty"`
	// KeelBranches is a keel-branches critic's state, opaque to the core
	// (the plugin's keelbranch.State).
	KeelBranches json.RawMessage `json:"keel_branches,omitempty"`

	// Scout is a scout's state, opaque to the core (the plugin's
	// scout.State): the search cursor, each tracked PR's and issue's head
	// and entry digest, path verdicts, test counts, ledger outbox times.
	Scout json.RawMessage `json:"scout,omitempty"`
	// GHCache is a github-cache critic's state, opaque to the core (the
	// plugin's ghcache.State): per entity marks, tier and next due time.
	GHCache json.RawMessage `json:"gh_cache,omitempty"`

	// BandHub is a band-hub mirror's state, opaque to the core (the plugin's
	// bandstate.State): the view it last wrote and its cached branch tips.
	BandHub json.RawMessage `json:"band_hub,omitempty"`
	// Roster is the roster plugin's state, opaque to the core: the keys it
	// last wrote.
	Roster json.RawMessage `json:"roster,omitempty"`

	// OutboxWatermark is the form version up to which every post has
	// reached an end (outbox.go).
	OutboxWatermark uint64 `json:"outbox_watermark,omitempty"`

	// GoneStreak counts consecutive "not found" rounds per PR key.
	GoneStreak map[string]int `json:"gone_streak,omitempty"`

	// Probe is each PR's cheap fingerprint as of its last full read (PR
	// key -> digest), and FullReadAt when every PR was last read in full: a
	// review critic reads a PR in full only when its fingerprint moved or
	// the full-read interval passed (GHE GraphQL quota, 2026-11-17).
	Probe      map[string]string `json:"probe,omitempty"`
	FullReadAt *time.Time        `json:"full_read_at,omitempty"`

	// Teams is a teams critic's seen-map and gone tracking.
	Teams  *TeamsState  `json:"teams,omitempty"`
	Author *AuthorState `json:"author,omitempty"`
	Keel   *KeelState   `json:"keel,omitempty"`

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
	// EverySec is the critic's poll interval: ebacd runs a round this long
	// after the last one ends; 0 means the default (5 minutes).
	EverySec int `json:"every_sec,omitempty"`
	// Priority is the critic's budget class when its credential's bucket is
	// short: wake-critical, scan or backfill ("" = by source, sched.ClassOf).
	Priority string `json:"priority,omitempty"`

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
	// ApprovePRs are the only "owner/name#n@<head sha>" this critic may
	// approve, from the trusted config only (Authority.Apply).
	ApprovePRs []string `json:"approve_prs,omitempty"`
	// GHWrite is a github-cache critic's write scope and fields, from the
	// trusted config only (Authority.Apply).
	GHWrite *GHWrite `json:"gh_write,omitempty"`
	// Maintainer is the role this critic's alarms wake (a sync alarm, a
	// blocked approval); AutomatedMarkers make a REQUEST_CHANGES or blocking
	// thread automated (ApproveBlockers). Both from the trusted config only.
	Maintainer       string   `json:"maintainer,omitempty"`
	AutomatedMarkers []string `json:"automated_markers,omitempty"`

	// Worktree, when set, is where a reviewer checks out each PR head:
	// <Root>/<critic>-pr<N>, fetched through Clone.
	Worktree *Worktree `json:"worktree,omitempty"`

	// Notes names repo-specific review guidance the seat reads first.
	Notes string `json:"notes,omitempty"`

	FigaroSock string `json:"figaro_socket,omitempty"`
	// FormIgnored names the form-stored fields the trusted configuration
	// replaced (Trusted.Apply): ebac ls flags them.
	FormIgnored []string `json:"-"`
	// NoAuthority: neither the trusted config nor ebac's creation record
	// names this critic's authority; it is not run (fail closed, #11).
	NoAuthority bool `json:"-"`
	// Undeclared: ebac's configuration declares critics (registrations) and
	// not this one (rework step 3 flags it; a later switch archives it).
	Undeclared bool `json:"-"`

	// Source names the upstream this critic translates (source.go); empty
	// means github-pr.
	Source string `json:"source,omitempty"`

	// Chat is the Teams chat a teams critic watches (Mode teams only).
	Chat *ChatRef `json:"chat,omitempty"`
	// Author is an author watch's configuration (source github-author).
	Author *AuthorRef `json:"author_watch,omitempty"`
	// Keel is a keel-threads watch's configuration (source keel-threads).
	Keel *KeelRef `json:"keel,omitempty"`
	// Band is a band-hub mirror's configuration (source band-hub).
	Band *BandRef `json:"band_hub,omitempty"`
	// Releases is a release watch's configuration (source github-releases).
	Releases *ReleasesRef `json:"releases_watch,omitempty"`
	// BoardForm and InputsForm are a scout's two outside forms (source
	// github-scout): the board, read-only for the owner marks the merge
	// checklist reads, and the inputs form its owners keep (merge rule,
	// ledger acks) and whose ledger outbox it writes.
	BoardForm  string `json:"board_form,omitempty"`
	InputsForm string `json:"inputs_form,omitempty"`
	// Scout is a scout's configuration: repo, authors, relevance filter,
	// owner roles.
	Scout *ScoutWatch `json:"scout_watch,omitempty"`
	// Branches is a keel-branches critic's configuration (source
	// keel-branches).
	Branches *KeelBranches `json:"keel_branches,omitempty"`
	// Approve is an approve-requests critic's configuration (source
	// approve-requests): who may ask, in which chat, under which grant.
	Requests *ApproveWatch `json:"approve_requests,omitempty"`
	// OrgApprove configures an org-approve critic (SourceOrgApprove).
	OrgApprove *OrgApproveWatch `json:"org_approve,omitempty"`
	// PingEverySec, when set, is COLLECT mode: events are gathered every
	// round, and the seat is pinged at most this often, one line, only when
	// something wake-worthy arrived since the last ping (W7 item 2).
	PingEverySec int `json:"ping_every_sec,omitempty"`

	Stop    StopConditions `json:"stop"`
	Runtime Runtime        `json:"runtime"`

	// Trigger is the role form's key trigger as read with the critic: the
	// rules, owned by whoever holds the role, never part of the config.
	Trigger json.RawMessage `json:"-"`
	// Reads are the role-form keys in SourceReads[source], as read with the
	// critic: the plugin's holder-edited configuration (Request.Keys).
	Reads map[string]json.RawMessage `json:"-"`
	// LegacyQuiet is a pre-trigger chat.quiet list, read only so the first
	// round of a trigger-aware binary can turn it into rules.
	LegacyQuiet []string `json:"-"`
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

// DefaultEvery is a new PR critic's poll interval when none is given: 30 min
// for a reviewer (a one-PR critic at 5 min cost 12 GraphQL points/h, GHE quota
// 11-18), 5 min otherwise.
func DefaultEvery(m Mode) time.Duration {
	if m == ModeReviewer {
		return 30 * time.Minute
	}
	return 5 * time.Minute
}

// Interval is the critic's poll interval: EverySec, else the default 5 minutes.
func (w *Critic) Interval() time.Duration {
	if w.EverySec > 0 {
		return time.Duration(w.EverySec) * time.Second
	}
	return 5 * time.Minute
}

// SourceName is the critic's source; critics made before sources existed are
// GitHub PR critics.
func (w *Critic) SourceName() string {
	if w.Source != "" {
		return w.Source
	}
	if w.Mode == ModeTeams {
		return "teams-chat"
	}
	if w.Mode == ModeAuthor {
		return "github-author"
	}
	if w.Mode == ModeKeel {
		return "keel-threads"
	}
	return "github-pr"
}

// alertTarget is who an author or keel watch wakes: the configured role, else
// the critic's own role form.
func (w *Critic) AlertTarget() string {
	if w.Author != nil && w.Author.AlertRole != "" {
		return w.Author.AlertRole
	}
	if w.Keel != nil && w.Keel.AlertRole != "" {
		return w.Keel.AlertRole
	}
	if w.Releases != nil && w.Releases.AlertRole != "" {
		return w.Releases.AlertRole
	}
	return w.FormID
}

// ArchivedFormPrefix names a retired critic's role form. Enumeration skips
// such rows from the list alone: every form read renews figaro's keepalive,
// so a retired form that is still read never lapses (28 alive on 2026-11-05).
const ArchivedFormPrefix = "ebac-archived-"

func ValidName(n string) error {
	if n == "" {
		return fmt.Errorf("critic name is required")
	}
	if len(n) > 64 {
		return fmt.Errorf("critic name too long")
	}
	if strings.HasPrefix("ebac-"+n, ArchivedFormPrefix) {
		return fmt.Errorf("critic name %q: the archived- prefix is reserved for retired role forms", n)
	}
	for _, r := range n {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("critic name %q: use only [A-Za-z0-9_-] (it becomes a systemd instance name)", n)
		}
	}
	return nil
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

// AuthorRef is an author watch's configuration.
type AuthorRef struct {
	Login string     `json:"login"`
	Since time.Time  `json:"since"`
	Until *time.Time `json:"until,omitempty"`
	Repos []string   `json:"repos"`
	// AlertRole, when set, is the role (@form) woken instead of the critic's
	// own role form; its target-aria is resolved at every send, so the
	// alert follows that role's succession without a recast.
	AlertRole string `json:"alert_role,omitempty"`
	// Recent is how many of the author's most recently updated PRs the watch
	// follows (ebac add-author --recent); 0 is DefaultAuthorRecent.
	Recent int `json:"recent,omitempty"`
}

// RecentWindow is how many recent items a watch keeps on its form: an author
// watch's PRs, and a PR critic's closed PRs within the archival grace.
const RecentWindow = 50

// DefaultAuthorRecent is an author watch's window when none is configured.
const DefaultAuthorRecent = RecentWindow

// ArchiveGrace is how long a closed or merged PR stays: the critic's own
// archival grace, else the default one.
func (w *Critic) ArchiveGrace() time.Duration {
	if w.Stop.ArchiveGraceSec > 0 {
		return time.Duration(w.Stop.ArchiveGraceSec) * time.Second
	}
	return time.Duration(DefaultStop().ArchiveGraceSec) * time.Second
}

// Window is the number of recent PRs the watch follows.
func (a *AuthorRef) Window() int {
	if a.Recent > 0 {
		return a.Recent
	}
	return DefaultAuthorRecent
}

// AuthorState lives under system.ebac.runtime: what each PR in the recent
// window looked like last round, and a digest of the authored key as last
// set, so an unchanged window is not written again.
//
// The relevance filter (role-form keys paths, title_match) adds: the last
// good filter, the error last written to the form's filter_error key, a
// digest of the filter the verdicts were made under, and the path verdicts
// per PR at a head: Hit (its files matched) and Skip (untracked; the only
// trace an untracked PR leaves).
type AuthorState struct {
	Seen   map[string]AuthorSeen `json:"seen,omitempty"`
	Window string                `json:"window,omitempty"`

	Filter    *AuthorFilter     `json:"filter,omitempty"`
	FilterErr string            `json:"filter_err,omitempty"`
	FilterSum string            `json:"filter_sum,omitempty"`
	Hit       map[string]string `json:"hit,omitempty"`  // PR key -> head (12 hex)
	Skip      map[string]string `json:"skip,omitempty"` // PR key -> head (12 hex)
}

// AuthorFilter is an author watch's relevance filter: a PR is tracked when
// its title matches TitleMatch or any changed file matches one of Paths.
// Neither set tracks every PR.
type AuthorFilter struct {
	Paths      []string `json:"paths,omitempty"`
	TitleMatch string   `json:"title_match,omitempty"`
}

// Author filter form keys, owned by the role holder (figaro set), read by the
// plugin every round.
const (
	AuthorPathsKey  = "paths"
	AuthorTitleKey  = "title_match"
	AuthorFilterErr = "filter_error"
)

// SourceReads are, per source, the role-form keys the core hands its plugin
// on every round (Request.Keys): configuration the role holder edits.
var SourceReads = map[string][]string{
	"github-author": {AuthorPathsKey, AuthorTitleKey},
	SourceGHCache:   {GHCacheConfigKey, GHCacheOursKey},
	"keel-threads":  {KeelThreadsScopeKey, KeelInboxScopeKey, KeelMirrorKey, "review", "verdict", "review_state"},
}

// The keel watch's scope (Boss 2026-11-16): holder-edited role-form keys,
// read every round, that choose which rows and fields of a round the form
// gets. Absent means everything, as before scopes existed. They shape only
// what is written: the diff and the wake decision always see the full fetch.
const (
	KeelThreadsScopeKey = "threads_scope" // all | changed
	KeelInboxScopeKey   = "inbox_scope"   // all | heads | last:N
	KeelMirrorKey       = "mirror"        // KEY[.FIELD],... of threads and inbox
	KeelScopeErr        = "scope_error"
)

// KeelScope is a keel watch's scope as written in its keys.
type KeelScope struct {
	Threads string   `json:"threads,omitempty"`
	Inbox   string   `json:"inbox,omitempty"`
	Mirror  []string `json:"mirror,omitempty"`
}

func (s KeelScope) IsZero() bool { return s.Threads == "" && s.Inbox == "" && len(s.Mirror) == 0 }

type AuthorSeen struct {
	State string `json:"s"`
	Draft bool   `json:"d,omitempty"`
	Head  string `json:"h"`
}

type ChatRef struct {
	ID    string `json:"id"`
	Topic string `json:"topic,omitempty"`
	// Self is the Teams sender id our seats post as (8:orgid:...). Its new
	// messages are written compact and its edits and deletes not at all:
	// the seat wrote them. Empty: every sender is treated alike.
	Self string `json:"self,omitempty"`
}

const ModeAuthor Mode = "author"

const ModeTeams Mode = "teams"

// TeamsState lives under system.ebac.runtime: the seat never sees it.
type TeamsState struct {
	Seen       map[string]string `json:"seen,omitempty"` // message id -> content digest
	GoneStreak int               `json:"gone_streak,omitempty"`
	GoneSince  string            `json:"gone_since,omitempty"`
	// FailSince is the start of the current run of failed rounds of any kind;
	// Blind is set once that run passes teamsBlindAfter and the seat was told.
	// Names maps a sender id (8:orgid:...) to the display name seen on another
	// row: Teams leaves imdisplayname blank on some rows (all of a self-chat).
	Names     map[string]string `json:"names,omitempty"`
	FailSince string            `json:"fail_since,omitempty"`
	Blind     bool              `json:"blind,omitempty"`
	BlindErr  string            `json:"blind_err,omitempty"`
}

const ModeKeel Mode = "keel"

// KeelRef is a keel-threads watch: one keel repo's comment threads, read
// through ebac's own unix socket door (read-only, GET threads only).
type KeelRef struct {
	Repo  string   `json:"repo"`            // owner/name; the viewer id is owner-name
	Paths []string `json:"paths,omitempty"` // globs; empty = every path
	// WaitingOn are the authors whose new or edited comment in an open
	// thread wakes the seat (display name, account or alias).
	WaitingOn []string `json:"waiting_on"`
	// Self are the seat's own names: their comments are never announced.
	Self      []string `json:"self,omitempty"`
	AlertRole string   `json:"alert_role,omitempty"`
	Feed      bool     `json:"feed,omitempty"`
	URL       string   `json:"url,omitempty"`
	TokenCmd  []string `json:"token_cmd,omitempty"`
}

// KeelPathMatches applies a keel critic's path globs: path.Match, and dir/**
// for a subtree; no globs = every path.
func KeelPathMatches(p string, globs []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if dir, ok := strings.CutSuffix(g, "/**"); ok && strings.HasPrefix(p, dir+"/") {
			return true
		}
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

// KeelState lives under system.ebac.runtime: the seat never sees it.
type KeelState struct {
	Seen        map[string]string `json:"seen,omitempty"`     // comment id -> body digest
	Resolved    map[string]bool   `json:"resolved,omitempty"` // thread id -> resolved
	GoneStreak  int               `json:"gone_streak,omitempty"`
	GoneSince   string            `json:"gone_since,omitempty"`
	FailSince   string            `json:"fail_since,omitempty"`
	Blind       bool              `json:"blind,omitempty"`
	BlindErr    string            `json:"blind_err,omitempty"`
	Cursor      int64             `json:"cursor,omitempty"`
	FeedStarted bool              `json:"feed_started,omitempty"`
	// Scope is the last good scope (nil: everything); ScopeErr the error
	// shown in the form's key scope_error ("" when the keys are good).
	Scope    *KeelScope `json:"scope,omitempty"`
	ScopeErr string     `json:"scope_error,omitempty"`
	// ReviewErr is the review_error last written.
	ReviewErr string `json:"review_err,omitempty"`
}

// EndWarnBefore: an expires_after end is announced this long ahead, once per
// expiry, so its owner can renew it in time.
const EndWarnBefore = 3 * 24 * time.Hour

// ExpiresAt is when an expires_after end falls (false: no such end).
func (w *Critic) ExpiresAt() (time.Time, bool) {
	if w.Stop.ExpiresAfterSec <= 0 {
		return time.Time{}, false
	}
	base := w.CreatedAt
	if w.Stop.RenewedAt != nil {
		base = *w.Stop.RenewedAt
	}
	return base.Add(time.Duration(w.Stop.ExpiresAfterSec) * time.Second), true
}

// EndReason says why the critic has reached its end at now, or "". Every
// critic, whatever its source: a deadline (until), an expiry (expires_after,
// renewable), or every PR closed plus the grace (critics that hold PRs; the
// PR round keeps AllClosedSince). ebacd ends it: archive, then its forms go.
func (w *Critic) EndReason(now time.Time) string {
	if d := w.Stop.Deadline; d != nil && !now.Before(*d) {
		return "until " + d.UTC().Format(time.RFC3339) + " passed"
	}
	if at, ok := w.ExpiresAt(); ok && !now.Before(at) {
		since := "created"
		if w.Stop.RenewedAt != nil {
			since = "renewed"
		}
		return fmt.Sprintf("expired: %s since %s", time.Duration(w.Stop.ExpiresAfterSec)*time.Second, since)
	}
	if w.Stop.UntilAllClosed && len(w.PRs) > 0 && w.Runtime.AllClosedSince != nil &&
		now.Sub(*w.Runtime.AllClosedSince) >= time.Duration(w.Stop.ArchiveGraceSec)*time.Second {
		return "all PRs closed + " + (time.Duration(w.Stop.ArchiveGraceSec) * time.Second).String() + " grace"
	}
	return ""
}

// EndWarning says whether an advance warning of the expiry is due now: inside
// EndWarnBefore of it, not ended yet, not warned for this expiry.
func (w *Critic) EndWarning(now time.Time) (time.Time, bool) {
	at, ok := w.ExpiresAt()
	if !ok || !now.Before(at) || at.Sub(now) > EndWarnBefore {
		return time.Time{}, false
	}
	if f := w.Runtime.EndWarnedFor; f != nil && f.Equal(at) {
		return time.Time{}, false
	}
	return at, true
}

// DefaultPurgeAfter: an archived critic's forms are killed this long after it
// was archived, unless purge_at says otherwise (nothing lives indefinitely).
const DefaultPurgeAfter = 7 * 24 * time.Hour

// PurgeDue says when an archived critic, not yet ended, is purged (false: not
// archived, or already ended). An archive with no recorded time is due now.
func (w *Critic) PurgeDue() (time.Time, bool) {
	if !w.Runtime.Archived || w.Runtime.EndedAt != nil {
		return time.Time{}, false
	}
	if p := w.Runtime.PurgeAt; p != nil {
		return *p, true
	}
	if w.Runtime.ArchivedAt.IsZero() {
		return time.Time{}, true
	}
	return w.Runtime.ArchivedAt.Add(DefaultPurgeAfter), true
}

// EndLabel is the critic's end for ebac ls: every end it has, short; "none".
func (w *Critic) EndLabel() string {
	if at, ok := w.PurgeDue(); ok {
		if at.IsZero() {
			return "purge now"
		}
		return "purge " + at.UTC().Format("2006-01-02")
	}
	var parts []string
	if r := w.Stop.WithRole; r != "" {
		parts = append(parts, "with-role "+shortID(r))
	}
	if d := w.Stop.Deadline; d != nil {
		parts = append(parts, "until "+d.UTC().Format("2006-01-02"))
	}
	if at, ok := w.ExpiresAt(); ok {
		parts = append(parts, "exp "+at.UTC().Format("2006-01-02"))
	}
	if w.Stop.UntilAllClosed && len(w.PRs) > 0 {
		parts = append(parts, "closed+"+shortDur(time.Duration(w.Stop.ArchiveGraceSec)*time.Second))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func shortDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return d.String()
}

// HardStop says why a critic must stop before it spends an upstream call, or "".
func (w *Critic) HardStop() string {
	if w.Stop.MaxRounds > 0 && w.Runtime.Round > w.Stop.MaxRounds {
		return fmt.Sprintf("max rounds (%d) reached", w.Stop.MaxRounds)
	}
	if w.Stop.MaxConsecutiveFailures > 0 && w.Runtime.ConsecutiveFailures >= w.Stop.MaxConsecutiveFailures {
		return fmt.Sprintf("%d consecutive failures (last: %s)", w.Runtime.ConsecutiveFailures, w.Runtime.LastError)
	}
	return ""
}

// SourceBandHub mirrors the band hub into a studied form. It never wakes
// anyone, so its role needs no seat.
const SourceBandHub = "band-hub"

// NeedsSeat says whether an empty role is a fault: a mirror is read by its
// studiers and wakes nobody.
func (w *Critic) NeedsSeat() bool {
	switch w.SourceName() {
	case SourceBandHub, SourceRoster, SourceScout, SourceKeelBranches:
		return false // they ping the roles they name (or their child forms' holders), never a seat of their own
	}
	return true
}

// SourceRoster derives the figaro family's role registry (roles, holders,
// lieutenants, live transient forks) from the aria store into a studied
// form. Like the band-hub mirror it has no seat and never wakes anyone.
const SourceRoster = "aria-roster"

// Aria is one row of the aria store as the core hands it to the roster
// plugin: the fields the roster reads, copied by name from figaro's list.
type Aria struct {
	ID            string `json:"id"`
	Handle        string `json:"handle,omitempty"`
	Kind          string `json:"kind,omitempty"` // conversation | form
	State         string `json:"state,omitempty"`
	Model         string `json:"model,omitempty"`
	Mantra        string `json:"mantra,omitempty"`
	Parent        string `json:"parent,omitempty"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	ContextLimit  int    `json:"context_limit,omitempty"`
	TTLMS         int64  `json:"ttl_ms,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`  // unix millis
	ReapedAt      int64  `json:"reaped_at,omitempty"`   // unix millis; set: the aria is reaped (listed, not live)
	Name          string `json:"name,omitempty"`        // forms
	TargetAria    string `json:"target_aria,omitempty"` // a role's holder
}

// BandRef is a band-hub mirror's configuration.
type BandRef struct {
	// Socket is the hub's unix socket (metadata-only reader access).
	Socket string `json:"socket"`
	// Utility names the hub's utility nodes; every other node is heavy.
	Utility []string `json:"utility,omitempty"`
	// Repo is where the jobs' branch params live, for the superseded check.
	Repo string `json:"repo,omitempty"`
	// LoadEverySec is the heartbeat cadence of the load keys (queues,
	// node_load, updated); 0 is bandstate.UpdatedEvery (10 min).
	LoadEverySec int `json:"load_every_sec,omitempty"`
}

// SourceReleases watches one GitHub repo's releases (served by ebac-gh).
const SourceReleases = "github-releases"

// ReleasesRef is a release watch's configuration.
type ReleasesRef struct {
	Repo string `json:"repo"` // owner/name on github.com
	// AlertRole, when set, is the role woken instead of the critic's own.
	AlertRole string `json:"alert_role,omitempty"`
	// API is the REST base, https://api.github.com unless set (tests).
	API string `json:"api,omitempty"`
}

// ReleasesState lists the tags seen, newest first, bounded.
type ReleasesState struct {
	Seen []string `json:"seen,omitempty"`
	// Armed is set by the first round, which only records: a repo with no
	// release yet must still announce its first one.
	Armed bool `json:"armed,omitempty"`
	// ETag is the list's last validator: the next request is conditional,
	// and a 304 does not count against GitHub's unauthenticated limit
	// (60/h per IP; gluk's egress IP is shared).
	ETag string `json:"etag,omitempty"`
	// LimitedUntil is GitHub's x-ratelimit-reset after a rate-limit refusal:
	// no request before it. A back-off, not a failure.
	LimitedUntil time.Time `json:"limited_until,omitempty"`
	// Remaining is x-ratelimit-remaining as last answered (nil: not sent).
	Remaining *int `json:"remaining,omitempty"`
}

// PingEvery is the collect-mode ping cadence; 0 is ordinary delivery.
func (w *Critic) PingEvery() time.Duration { return time.Duration(w.PingEverySec) * time.Second }

// MaxLastError bounds runtime.last_error on the form: a GraphQL failure that
// carried its whole query and every error line was 42.5 KB of one critic's
// form (2026-11-12, scout @6f376a71 at 184 KB of a 192 KB budget).
const MaxLastError = 1024

// ClipError is an error as the form keeps it: its first line (at most
// MaxLastError bytes, cut on a rune boundary) and how many more lines it had,
// and how many of them repeat the first.
func ClipError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	first := strings.TrimSpace(lines[0])
	more, same := 0, 0
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		more++
		if l == first {
			same++
		}
	}
	tail := ""
	if more > 0 {
		tail = fmt.Sprintf(" [+%d more line(s), %d repeating this one]", more, same)
	}
	if room := MaxLastError - len(tail); len(first) > room {
		cut := room - len("…")
		for cut > 0 && !utf8.RuneStart(first[cut]) {
			cut--
		}
		first = first[:cut] + "…"
	}
	return first + tail
}

// CriticKey is the role-form key a critic's configuration lives in, and
// CriticErrorKey the key that says why a form's critic key is unusable.
const (
	CriticKey      = "critic"
	CriticErrorKey = "critic_error"
)

// CheckCriticKey says whether raw is a usable critic key: a JSON object that
// names its critic. Every write of the key passes through it (the 2026-11-10
// wipe left critic: {} on four forms and they vanished from ebac ls).
func CheckCriticKey(raw json.RawMessage) (*Critic, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("critic key absent")
	}
	var w Critic
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("critic key unreadable: %w", err)
	}
	if w.Name == "" {
		return nil, fmt.Errorf("critic key names no critic: %.80s", raw)
	}
	return &w, nil
}

// SourceScout is the GitHub scout (ebac-gh): one entry per relevant PR and
// issue of a few authors in one repo, as keys pr:<n> / issue:<n> on its own
// role form, the merge checklist's first unmet step, ledger records through
// the inputs form's outbox, and material issue changes pinged to the owner
// role of each PR linking the issue.
const SourceScout = "github-scout"

// ScoutWatch is a scout's configuration.
type ScoutWatch struct {
	Repo    string   `json:"repo"`    // owner/name on the critic's host
	Authors []string `json:"authors"` // logins whose PRs and issues are read
	// The relevance filter: a PR is tracked when it carries one of Labels,
	// its title matches TitleMatch, or a changed file matches one of Paths
	// (relevance.Spec). An issue is tracked on a label or title hit, or when
	// a tracked PR's body links it.
	Labels     []string `json:"labels,omitempty"`
	TitleMatch string   `json:"title_match,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	// Owners route a material issue change to the owner role of each PR
	// linking the issue: the first rule matching the PR's author (and title,
	// when set) names the role.
	Owners []OwnerRule `json:"owners,omitempty"`
	// WarnTo is the role (@form) pinged once when open entries alone pass
	// the form cap (the Decisions Lead).
	WarnTo string `json:"warn_to,omitempty"`
	// Claims (Praxis): an issue in the repo is a claim when ClaimBy posted a
	// comment containing ClaimPhrase on it, or it carries ClaimLabel; it is
	// written as claim:<issue>. Empty ClaimBy and ClaimLabel: no claims.
	ClaimBy     string `json:"claim_by,omitempty"`
	ClaimPhrase string `json:"claim_phrase,omitempty"`
	ClaimLabel  string `json:"claim_label,omitempty"`
	// FilesTitle: an open PR whose title matches it carries its changed
	// files on its entry (fs).
	FilesTitle string `json:"files_title,omitempty"`
	// FilesAuthor are the authors whose matching PRs carry fs (empty: the
	// first of Authors, ours).
	FilesAuthor []string `json:"files_author,omitempty"`
}

// OwnerRule maps a PR to its owner role: author, optionally a title regex.
type OwnerRule struct {
	Author     string `json:"author"`
	TitleMatch string `json:"title_match,omitempty"`
	Role       string `json:"role"` // @<form id>
}

// SourceKeelBranches follows one keel repo's work/* branches (served by
// ebac-keel): ONE child form per branch, holding its facts (head, base,
// merge base, diff stat, files, commits, review threads, reviewers'
// verdicts). The branch's builder holds the form (its target-aria) and owns
// code_ready and ready on it; the plugin never writes them.
const SourceKeelBranches = "keel-branches"

// SourceGHCache is the GitHub cache (ebac-gh, slice 1: PR read side): one
// child form per tracked PR (ebac-<critic>-<repo>-pr-<n>, keys gh, parsed,
// sync), tracked by the rules in the role form's holder-edited config key;
// the plugin writes its index to the role form's track key.
const SourceGHCache = "github-cache"

// GHCacheConfigKey is the github-cache role form's holder-edited config:
// {repos[], rules[{author[], label[], tier}], ttl}.
const GHCacheConfigKey = "config"

// GHCacheOursKey is the register's requests (ours.praxis_new.<id>: a new
// issue, written by the issue CLI only); GHCacheSyncKey is the sync's answer
// on the register (sync.praxis_new.<id>, sync.alarm.*), written by the
// plugin only.
const (
	GHCacheOursKey = "ours"
	GHCacheSyncKey = "sync"
)

// KeelBranches is a keel-branches critic's configuration.
type KeelBranches struct {
	Repo   string `json:"repo"`             // keel owner/name, e.g. aether/aether
	Base   string `json:"base,omitempty"`   // the branch work is measured against; "" is main
	Prefix string `json:"prefix,omitempty"` // "" is work/
	// Reviewer is the role (@form) pinged on code_ready and on each new head
	// once code_ready is set.
	Reviewer string `json:"reviewer,omitempty"`
	// URL and TokenCmd read keel over HTTPS; unset reads ebac's keel door.
	URL      string   `json:"url,omitempty"`
	TokenCmd []string `json:"token_cmd,omitempty"`
	// MaxThreadReads caps a pass's threads reads of DUE branches (0: 8);
	// SweepReads caps the sweep's reads in the slots they leave (0: 2), so a
	// quiet pass costs refs, the feed and at most SweepReads reads.
	MaxThreadReads int `json:"max_thread_reads,omitempty"`
	SweepReads     int `json:"sweep_reads,omitempty"`
}

// ChildLease is a child form's lifetime (its system.ttl), renewed each pass:
// a stuck or removed critic leaks no form for longer than this.
const ChildLease = 2 * 24 * time.Hour

// MaxChildren caps a critic's child forms: a write past it is refused whole.
const MaxChildren = 100

// ChildOwnerKeys are the keys a child form's holder owns: the core passes
// them to the plugin each round and refuses any plugin write of them.
var ChildOwnerKeys = []string{"code_ready", "ready"}

// GHCacheChildReadKeys are the entity-form keys the core reads for the
// GitHub cache each round: gh (seats edit its write fields) and sync (seats
// ack its alarms). The plugin writes both, guarded by FormWrite.Expect.
var GHCacheChildReadKeys = []string{"gh", "sync", "want", "praxis"}

// ChildMintOnly are child-form keys a plugin may set only in the write that
// mints the form (praxis: an issue's praxis keys, the issue CLI's afterwards).
var ChildMintOnly = map[string]bool{"praxis": true}

// ChildWriteRefused are the child-form keys no plugin may write: the owner's
// keys and the form's own (runtime: figaro 0.75's daemon-written keys).
// pr_url and pr_number are keel CI's (K5: the PR it opened for the branch).
var ChildWriteRefused = map[string]bool{"code_ready": true, "ready": true, "pr_url": true, "pr_number": true, "system": true, "runtime": true,
	"target-aria": true, "name": true, "kind": true, CriticKey: true}

// shortID is a form address cut to @ and 8 hex.
func shortID(id string) string {
	if len(id) > 9 {
		return id[:9]
	}
	return id
}
