package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PRKey identifies a pull request across repos: "owner/repo#number".
type PRKey string

func MakePRKey(owner, repo string, number int) PRKey {
	return PRKey(fmt.Sprintf("%s/%s#%d", owner, repo, number))
}

// Snapshot is the full canonical state of one critic at one instant.
// It lives on disk and is NEVER projected wholesale into a form: a studied
// form is mirrored untruncated into the model's context, so the form gets
// counts and ids while this gets bodies.
type Snapshot struct {
	Critic  string             `json:"critic"`
	Round   int                `json:"round"`
	TakenAt time.Time          `json:"taken_at"`
	PRs     map[PRKey]*PRState `json:"prs"`

	// Complete is false when any page of any list was not retrieved.
	// A partial scan must never be allowed to infer a deletion: prangl
	// learned this the expensive way. The differ checks it.
	Complete bool   `json:"complete"`
	Note     string `json:"note,omitempty"`
}

type PRState struct {
	Key       PRKey  `json:"key"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	State     string `json:"state"` // OPEN | CLOSED | MERGED
	IsDraft   bool   `json:"is_draft"`
	HeadSHA   string `json:"head_sha"`
	BaseRef   string `json:"base_ref"`
	UpdatedAt string `json:"updated_at"`

	ReviewDecision string `json:"review_decision,omitempty"`

	Threads map[string]*Thread `json:"threads"`
	Reviews map[string]*Review `json:"reviews"`

	// Comments are top-level PR conversation comments (GraphQL
	// pullRequest.comments), distinct from review-thread comments in
	// Threads. A reply that never touches a line of diff — the common
	// shape for "here is commit 2 of 4" or "did you see this?" — lives
	// here, not in a Thread, and was invisible to the differ until this
	// field existed: FetchPR never asked GraphQL for it.
	Comments map[string]*Comment `json:"comments"`

	// Gate state for cheap polling.
	ETag string `json:"etag,omitempty"`

	// Complete is per-PR: one PR's pagination failure must not poison
	// the other PRs in the same critic.
	Complete bool `json:"complete"`
}

func (p *PRState) Closed() bool {
	return p.State == "CLOSED" || p.State == "MERGED"
}

type Thread struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Line       int        `json:"line"`
	IsResolved bool       `json:"is_resolved"`
	IsOutdated bool       `json:"is_outdated"`
	Comments   []*Comment `json:"comments"`
}

// LastComment returns the most recent comment, or nil for an empty thread.
func (t *Thread) LastComment() *Comment {
	if len(t.Comments) == 0 {
		return nil
	}
	return t.Comments[len(t.Comments)-1]
}

// FirstComment returns the comment that actually opened the thread, or nil
// for an empty thread. Distinct from LastComment: when a thread is adopted
// with more than one comment already in it (the opener and a same-gap reply
// both landed between one poll and the next -- routine when a human replies
// within minutes of a finding), LastComment silently attributes the whole
// thread to whoever replied, not whoever raised it.
func (t *Thread) FirstComment() *Comment {
	if len(t.Comments) == 0 {
		return nil
	}
	return t.Comments[0]
}

// AwaitingUs is true when the last word in an unresolved thread was not ours.
func (t *Thread) AwaitingUs(me string) bool {
	if t.IsResolved {
		return false
	}
	last := t.LastComment()
	return last != nil && !strings.EqualFold(last.Author, me)
}

type Comment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	IsBot     bool   `json:"is_bot"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	URL       string `json:"url"`
	// Digest fingerprints the body so an edit is detectable without
	// diffing prose. UpdatedAt alone is not enough: some edits do not
	// move it, and some non-edits do.
	Digest string `json:"digest"`
}

func (c *Comment) computeDigest() {
	sum := sha256.Sum256([]byte(c.Body))
	c.Digest = hex.EncodeToString(sum[:8])
}

type Review struct {
	ID          string `json:"id"`
	Author      string `json:"author"`
	IsBot       bool   `json:"is_bot"`
	State       string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	SubmittedAt string `json:"submitted_at"`
	Body        string `json:"body"`
}

// SortedPRKeys gives deterministic iteration. Every projection and every
// digest depends on this: an unstable order makes an unchanged snapshot
// look changed and burns a turn per poll.
func (s *Snapshot) SortedPRKeys() []PRKey {
	keys := make([]PRKey, 0, len(s.PRs))
	for k := range s.PRs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (p *PRState) SortedThreadIDs() []string {
	ids := make([]string, 0, len(p.Threads))
	for id := range p.Threads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (p *PRState) SortedCommentIDs() []string {
	ids := make([]string, 0, len(p.Comments))
	for id := range p.Comments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Counts summarises one PR for the form projection.
type Counts struct {
	Threads     int `json:"threads"`
	Unresolved  int `json:"unresolved"`
	AwaitingUs  int `json:"awaiting_us"`
	HumanThread int `json:"human_unresolved"`
}

func (p *PRState) Counts(me string) Counts {
	var c Counts
	for _, t := range p.Threads {
		c.Threads++
		if t.IsResolved {
			continue
		}
		c.Unresolved++
		if t.AwaitingUs(me) {
			c.AwaitingUs++
		}
		for _, cm := range t.Comments {
			if !cm.IsBot {
				c.HumanThread++
				break
			}
		}
	}
	return c
}
