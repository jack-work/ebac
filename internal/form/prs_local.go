package form

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/gluck/ebac/internal/model"
)

// A review critic's PR detail (every thread, review and comment, as identity
// and digest) is what the next round diffs against. It was ~45 KB on one form
// on 2026-11-17, and a form is delivered whole to every new or catching-up
// studier, so it lives on disk beside the scout state (runtime_local.go):
//
//   - the form keeps prs.<pr> as a head (prHead): identity, state, heads and
//     counts, what a studier or the board reads;
//   - <Root>/runtime/<form>.prs.json keeps the digest tree (formPR), written
//     by SaveRound after its commit lands, so it is never ahead of the form;
//     a failed write leaves it one round behind, and the next round repeats
//     that round's events rather than missing them;
//   - Snapshot reads the tree from the form while the form still carries one
//     (written before this change, or by an older ebac: the one-time
//     migration, as the first save writes heads and the file), else from the
//     file, else from the heads alone with PRState.NoDetail, which the differ
//     re-baselines instead of reporting every thread as new.

// prsFile is the on-disk detail: the round it was taken in and the tree.
type prsFile struct {
	Round int             `json:"round"`
	PRs   json.RawMessage `json:"prs"`
}

// prHead is one PR as the form keeps it.
type prHead struct {
	Key            model.PRKey           `json:"key"`
	Owner          string                `json:"owner"`
	Repo           string                `json:"repo"`
	Number         int                   `json:"number"`
	URL            string                `json:"url"`
	Title          string                `json:"title"`
	Author         string                `json:"author"`
	State          string                `json:"state"`
	IsDraft        bool                  `json:"is_draft,omitempty"`
	HeadSHA        string                `json:"head_sha"`
	ReviewDecision string                `json:"review_decision,omitempty"`
	Complete       bool                  `json:"complete"`
	Pending        []model.PendingReview `json:"pending_reviews,omitempty"`
	N              prCounts              `json:"n"`
	// WaitingOn counts, per person, the unresolved threads of an open PR
	// that wait on them: the author when someone else spoke last, else the
	// thread's opener.
	WaitingOn map[string]int `json:"waiting_on,omitempty"`
}

type prCounts struct {
	Threads    int `json:"threads"`
	Unresolved int `json:"unresolved"`
	Comments   int `json:"comments"` // conversation and thread comments, bots included
	Bots       int `json:"bots"`     // of comments
	Reviews    int `json:"reviews"`
}

func prsHeads(s *model.Snapshot) map[string]*prHead {
	out := map[string]*prHead{}
	if s == nil {
		return out
	}
	for k, p := range s.PRs {
		h := &prHead{Key: p.Key, Owner: p.Owner, Repo: p.Repo, Number: p.Number, URL: p.URL, Title: p.Title,
			Author: p.Author, State: p.State, IsDraft: p.IsDraft, HeadSHA: p.HeadSHA, ReviewDecision: p.ReviewDecision,
			Complete: p.Complete, Pending: p.Pending}
		h.N.Reviews = len(p.Reviews)
		for _, c := range p.Comments {
			h.N.Comments++
			if c.IsBot {
				h.N.Bots++
			}
		}
		for _, th := range p.Threads {
			h.N.Threads++
			for _, c := range th.Comments {
				h.N.Comments++
				if c.IsBot {
					h.N.Bots++
				}
			}
			if th.IsResolved {
				continue
			}
			h.N.Unresolved++
			if who := waitingOn(p, th); who != "" && (p.State == "" || p.State == "OPEN") {
				if h.WaitingOn == nil {
					h.WaitingOn = map[string]int{}
				}
				h.WaitingOn[who]++
			}
		}
		out[PrFormKey(k)] = h
	}
	return out
}

// waitingOn is who an unresolved thread waits on: the PR author when the
// last published word is someone else's, else the thread's opener. Empty
// when the thread holds nothing published.
func waitingOn(p *model.PRState, th *model.Thread) string {
	var first, last string
	for _, c := range th.Comments {
		if c.State == "PENDING" {
			continue
		}
		if first == "" {
			first = c.Author
		}
		last = c.Author
	}
	if first == "" {
		return ""
	}
	if p.Author != "" && strings.EqualFold(last, p.Author) {
		return first
	}
	return p.Author
}

// roundPRs is what a round's snapshot puts on the form (the heads) and on
// disk (the detail).
func roundPRs(snap *model.Snapshot) (json.RawMessage, *prsFile) {
	return model.MustJSON(prsHeads(snap)), &prsFile{Round: snap.Round, PRs: model.MustJSON(prsTree(snap))}
}

// prsOf is the previous round's PRs: the form's tree while it has one, else
// the file's, else the form's heads marked NoDetail.
func prsOf(form json.RawMessage, local *prsFile) (map[model.PRKey]*model.PRState, error) {
	prs, err := prsFromTree(form)
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		if !p.NoDetail {
			return prs, nil // a tree: the form predates heads
		}
	}
	if local != nil {
		return prsFromTree(local.PRs)
	}
	return prs, nil
}

func (s *Store) prsPath(formID string) string {
	return filepath.Join(s.Root, localDir, strings.TrimPrefix(formID, "@")+".prs.json")
}

// readPRsLocal returns the form's PR detail from disk, or nil when there is
// none or it is unreadable (said on Warn).
func (s *Store) readPRsLocal(formID string) *prsFile {
	raw := s.readFile(s.prsPath(formID), "PR detail", "reading the form's heads alone")
	if raw == nil {
		return nil
	}
	var f prsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		s.warnf("ebac: PR detail %s: %v, reading the form's heads alone\n", s.prsPath(formID), err)
		return nil
	}
	return &f
}

// writePRsLocal replaces the form's PR detail on disk; nil writes nothing.
func (s *Store) writePRsLocal(formID string, f *prsFile) error {
	if f == nil {
		return nil
	}
	return writeAtomic(s.prsPath(formID), model.MustJSON(f))
}
