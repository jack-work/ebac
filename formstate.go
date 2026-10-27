package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/model"
	"github.com/jack-work/figaro/api/form"
	"github.com/jack-work/figaro/api/rpc"
)

// A critic's role form IS its state. Config, runtime, and the observed pull
// requests all live there, and every save is one patch diffed against the
// live form, so the form's history is the audit log of each PR as ebac saw it
// and the seat studying the form reads each round's change as its diff.
//
//	kind, critic                 config (seat-visible)
//	prs.<pr>                     observed PR state; lists keyed by GitHub id
//	harness, harness_protocol_N  the standing protocol
//	post.<id>                    the outbox (outbox.go)
//	system.ebac                  runtime, incl. the outbox watermark (never shown to seats)

const (
	criticKind        = "ebac.critic"
	protocolKeyPrefix = "harness_protocol_"
)

// ebacState is what ebac keeps under system.ebac.
type ebacState struct {
	Runtime  model.Runtime `json:"runtime"`
	Round    int           `json:"snapshot_round,omitempty"`
	TakenAt  time.Time     `json:"snapshot_taken_at,omitempty"`
	Complete bool          `json:"snapshot_complete,omitempty"`
	Note     string        `json:"snapshot_note,omitempty"`
}

// The form keeps what the next round compares against, never the text:
// figaro writes a form's whole state at the head of every log segment, so
// state is held under FormStateBudget. Bodies reach the seat through inbox.

// formItem is one comment or review: identity and digest, no body.
type formItem struct {
	By  string `json:"by"`
	D   string `json:"d"`
	I   int    `json:"i,omitempty"`   // position in its thread
	S   string `json:"s,omitempty"`   // review state
	P   bool   `json:"p,omitempty"`   // pending: not published
	Bot bool   `json:"bot,omitempty"` // bot author
	St  bool   `json:"st,omitempty"`  // bot verdict stamped under a person
}

type formThread struct {
	R bool                 `json:"r,omitempty"` // resolved
	O bool                 `json:"o,omitempty"` // outdated
	C map[string]*formItem `json:"c"`
}

type formPR struct {
	Key            model.PRKey            `json:"key"`
	Owner          string                 `json:"owner"`
	Repo           string                 `json:"repo"`
	Number         int                    `json:"number"`
	URL            string                 `json:"url"`
	Title          string                 `json:"title"`
	Author         string                 `json:"author"`
	State          string                 `json:"state"`
	IsDraft        bool                   `json:"is_draft,omitempty"`
	HeadSHA        string                 `json:"head_sha"`
	ReviewDecision string                 `json:"review_decision,omitempty"`
	Complete       bool                   `json:"complete"`
	Pending        []model.PendingReview  `json:"pending_reviews,omitempty"`
	Threads        map[string]*formThread `json:"threads"`
	Reviews        map[string]*formItem   `json:"reviews"`
	Comments       map[string]*formItem   `json:"comments"`
}

// prFormKey makes a PR key safe as one path segment.
func prFormKey(k model.PRKey) string { return strings.ReplaceAll(string(k), ".", "%2E") }

func prsTree(s *model.Snapshot) map[string]*formPR {
	out := map[string]*formPR{}
	if s == nil {
		return out
	}
	for k, p := range s.PRs {
		f := &formPR{Key: p.Key, Owner: p.Owner, Repo: p.Repo, Number: p.Number, URL: p.URL, Title: p.Title,
			Author: p.Author, State: p.State, IsDraft: p.IsDraft, HeadSHA: p.HeadSHA, ReviewDecision: p.ReviewDecision,
			Complete: p.Complete, Pending: p.Pending,
			Threads: map[string]*formThread{}, Reviews: map[string]*formItem{}, Comments: map[string]*formItem{}}
		for id, th := range p.Threads {
			t := &formThread{R: th.IsResolved, O: th.IsOutdated, C: map[string]*formItem{}}
			for i, c := range th.Comments {
				t.C[c.ID] = &formItem{By: c.Author, D: c.Digest, I: i, P: c.State == "PENDING", Bot: c.IsBot}
			}
			f.Threads[id] = t
		}
		for id, r := range p.Reviews {
			f.Reviews[id] = &formItem{By: r.Author, D: r.BodyDigest(), S: r.State, Bot: r.IsBot, St: r.Stamped}
		}
		for id, c := range p.Comments {
			f.Comments[id] = &formItem{By: c.Author, D: c.Digest, Bot: c.IsBot}
		}
		out[prFormKey(k)] = f
	}
	return out
}

// readItem accepts the compact item and the full objects ebac wrote before
// 2026-10-21, so a form is readable either side of its compaction.
type readItem struct {
	By, D, S        string
	I               *int
	P, Bot, St      bool
	Body, CreatedAt string
}

func (r *readItem) UnmarshalJSON(b []byte) error {
	var m struct {
		By      string `json:"by"`
		D       string `json:"d"`
		I       *int   `json:"i"`
		S       string `json:"s"`
		P       bool   `json:"p"`
		Bot     bool   `json:"bot"`
		St      bool   `json:"st"`
		Author  string `json:"author"`
		Digest  string `json:"digest"`
		Body    string `json:"body"`
		State   string `json:"state"`
		IsBot   bool   `json:"is_bot"`
		Stamped bool   `json:"stamped"`
		Created string `json:"created_at"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*r = readItem{By: model.FirstNonEmpty(m.By, m.Author), D: model.FirstNonEmpty(m.D, m.Digest), S: model.FirstNonEmpty(m.S, m.State),
		I: m.I, P: m.P || m.State == "PENDING", Bot: m.Bot || m.IsBot, St: m.St || m.Stamped, Body: m.Body, CreatedAt: m.Created}
	if r.D == "" && m.Body != "" {
		r.D = model.BodyDigest(m.Body)
	}
	return nil
}

type readThread struct {
	R, O       bool
	C          map[string]*readItem
	IsResolved bool                 `json:"is_resolved"`
	IsOutdated bool                 `json:"is_outdated"`
	Comments   map[string]*readItem `json:"comments"`
}

func (t *readThread) UnmarshalJSON(b []byte) error {
	var m struct {
		R          bool                 `json:"r"`
		O          bool                 `json:"o"`
		C          map[string]*readItem `json:"c"`
		IsResolved bool                 `json:"is_resolved"`
		IsOutdated bool                 `json:"is_outdated"`
		Comments   map[string]*readItem `json:"comments"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = readThread{R: m.R || m.IsResolved, O: m.O || m.IsOutdated, C: m.C}
	if t.C == nil {
		t.C = m.Comments
	}
	return nil
}

func prsFromTree(raw json.RawMessage) (map[model.PRKey]*model.PRState, error) {
	out := map[model.PRKey]*model.PRState{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var tree map[string]*struct {
		formPR
		Threads  map[string]*readThread `json:"threads"`
		Reviews  map[string]*readItem   `json:"reviews"`
		Comments map[string]*readItem   `json:"comments"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("prs: %w", err)
	}
	for _, f := range tree {
		// A member removed on the forge can come back as a null leaf rather
		// than an absent key; null means absent, at every level.
		if f == nil || f.Key == "" {
			continue
		}
		p := &model.PRState{Key: f.Key, Owner: f.Owner, Repo: f.Repo, Number: f.Number, URL: f.URL, Title: f.Title,
			Author: f.Author, State: f.State, IsDraft: f.IsDraft, HeadSHA: f.HeadSHA, ReviewDecision: f.ReviewDecision,
			Complete: f.Complete, Pending: f.formPR.Pending,
			Threads: map[string]*model.Thread{}, Reviews: map[string]*model.Review{}, Comments: map[string]*model.Comment{}}
		for id, t := range f.Threads {
			if t == nil {
				continue
			}
			th := &model.Thread{ID: id, IsResolved: t.R, IsOutdated: t.O}
			type ordered struct {
				c   *model.Comment
				pos int
				at  string
			}
			var cs []ordered
			for cid, it := range t.C {
				if it == nil {
					continue
				}
				c := &model.Comment{ID: cid, Author: it.By, Digest: it.D, IsBot: it.Bot, CreatedAt: it.CreatedAt}
				if it.P {
					c.State = "PENDING"
				}
				pos := -1
				if it.I != nil {
					pos = *it.I
				}
				cs = append(cs, ordered{c, pos, it.CreatedAt})
			}
			sort.Slice(cs, func(i, j int) bool {
				a, b := cs[i], cs[j]
				if a.pos != b.pos {
					return a.pos < b.pos
				}
				if a.at != b.at {
					return a.at < b.at
				}
				return a.c.ID < b.c.ID
			})
			for _, o := range cs {
				th.Comments = append(th.Comments, o.c)
			}
			p.Threads[id] = th
		}
		for id, it := range f.Reviews {
			if it != nil {
				p.Reviews[id] = &model.Review{ID: id, Author: it.By, State: it.S, Digest: it.D, IsBot: it.Bot, Stamped: it.St}
			}
		}
		for id, it := range f.Comments {
			if it != nil {
				p.Comments[id] = &model.Comment{ID: id, Author: it.By, Digest: it.D, IsBot: it.Bot, CreatedAt: it.CreatedAt}
			}
		}
		out[p.Key] = p
	}
	return out, nil
}

// criticForm is one read of a role form.
type criticForm struct {
	ID      string
	Version uint64
	Snap    form.Snapshot
	Keys    map[string]json.RawMessage
}

func (s *Store) readForm(ctx context.Context, id string) (*criticForm, error) {
	c, err := s.Fig.Node(ctx, id)
	if err != nil {
		return nil, err
	}
	resp, err := c.Form(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", id, figaro.Gone(err))
	}
	b, err := json.Marshal(resp.Snapshot)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}
	return &criticForm{ID: id, Version: resp.Version, Snap: resp.Snapshot, Keys: keys}, nil
}

func (f *criticForm) ebac() ebacState {
	var sys struct {
		Ebac ebacState `json:"ebac"`
	}
	_ = json.Unmarshal(f.Keys["system"], &sys)
	return sys.Ebac
}

func (f *criticForm) critic() (*model.Critic, error) {
	raw, ok := f.Keys["critic"]
	if !ok {
		return nil, fmt.Errorf("form %s has no critic", f.ID)
	}
	var w model.Critic
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("form %s critic: %w", f.ID, err)
	}
	if w.Name == "" {
		return nil, fmt.Errorf("form %s critic has no name", f.ID)
	}
	w.FormID = f.ID
	w.Runtime = f.ebac().Runtime
	return &w, nil
}

func (f *criticForm) snapshot() (*model.Snapshot, error) {
	e := f.ebac()
	prs, err := prsFromTree(f.Keys["prs"])
	if err != nil {
		return nil, fmt.Errorf("form %s: %w", f.ID, err)
	}
	if e.TakenAt.IsZero() && len(prs) == 0 {
		return nil, nil // never polled
	}
	name := ""
	if w, err := f.critic(); err == nil {
		name = w.Name
	}
	return &model.Snapshot{Critic: name, Round: e.Round, TakenAt: e.TakenAt, PRs: prs, Complete: e.Complete, Note: e.Note}, nil
}

// criticForms reads every form declaring kind ebac.critic.
func (s *Store) criticForms(ctx context.Context) ([]*criticForm, error) {
	ang, err := s.Fig.Angelus()
	if err != nil {
		return nil, err
	}
	resp, err := figaro.AuditAng(rpc.MethodList, "", ang).ListGlobal(ctx)
	if err != nil {
		return nil, fmt.Errorf("list forms: %w", err)
	}
	var out []*criticForm
	for _, r := range resp.Figaros {
		if r.Kind != "form" || !strings.HasPrefix(r.Name, "ebac-") {
			continue
		}
		f, err := s.readForm(ctx, r.ID)
		if errors.Is(err, figaro.ErrFormGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var kind string
		_ = json.Unmarshal(f.Keys["kind"], &kind)
		if kind == criticKind {
			out = append(out, f)
		}
	}
	return out, nil
}

// findCritic locates a critic's form by the name its critic key carries.
func (s *Store) findCritic(ctx context.Context, name string) (*criticForm, error) {
	if id, ok := s.formOf[name]; ok {
		f, err := s.readForm(ctx, id)
		if err == nil {
			if w, err := f.critic(); err == nil && w.Name == name {
				return f, nil
			}
		} else if !errors.Is(err, figaro.ErrFormGone) {
			return nil, err
		}
		delete(s.formOf, name)
	}
	forms, err := s.criticForms(ctx)
	if err != nil {
		return nil, err
	}
	var hits []*criticForm
	for _, f := range forms {
		if w, err := f.critic(); err == nil && w.Name == name {
			hits = append(hits, f)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no critic named %q (try: ebac ls)", name)
	case 1:
		s.formOf[name] = hits[0].ID
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return nil, fmt.Errorf("%d forms claim critic %q (%s): remove the stale ones", len(hits), name, strings.Join(ids, ", "))
}

func (s *Store) LoadCritic(ctx context.Context, name string) (*model.Critic, error) {
	f, err := s.findCritic(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.critic()
}

func (s *Store) LoadSnapshot(ctx context.Context, name string) (*model.Snapshot, error) {
	f, err := s.findCritic(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.snapshot()
}

func (s *Store) ListCritics(ctx context.Context) ([]*model.Critic, error) {
	forms, err := s.criticForms(ctx)
	if err != nil {
		return nil, err
	}
	var out []*model.Critic
	seen := map[string]string{}
	for _, f := range forms {
		w, err := f.critic()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ebac: unreadable critic form %s: %v\n", f.ID, err)
			continue
		}
		if other, dup := seen[w.Name]; dup {
			fmt.Fprintf(os.Stderr, "ebac: critic %s claimed by %s and %s\n", w.Name, other, f.ID)
			continue
		}
		seen[w.Name] = f.ID
		s.formOf[w.Name] = f.ID
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveCritic writes config and runtime.
func (s *Store) SaveCritic(ctx context.Context, w *model.Critic) error {
	return s.Save(ctx, w, nil, nil)
}

// Receipts are outbox fields to merge into post.<id>, by post id.
type Receipts map[string]map[string]json.RawMessage

// Save writes the critic, and the snapshot when one is given, as ONE patch,
// with any outbox receipts merged into the posts they answer.
func (s *Store) Save(ctx context.Context, w *model.Critic, snap *model.Snapshot, receipts Receipts) error {
	return s.SaveRound(ctx, w, snap, receipts, nil)
}

// SaveRound is Save plus the round's fresh inbox items (nil leaves the inbox).
func (s *Store) SaveRound(ctx context.Context, w *model.Critic, snap *model.Snapshot, receipts Receipts, fresh map[string]InboxItem) error {
	if w.FormID == "" {
		return fmt.Errorf("critic %s has no form", w.Name)
	}
	cfg := *w
	cfg.Runtime = model.Runtime{}
	cfgRaw, err := json.Marshal(criticConfig(&cfg))
	if err != nil {
		return err
	}
	set := map[string]json.RawMessage{
		"kind":   model.MustJSON(criticKind),
		"critic": cfgRaw,
	}
	harness, err := json.Marshal(harnessContract(w))
	if err != nil {
		return err
	}
	set["harness"] = harness
	s.formOf[w.Name] = w.FormID
	return s.commit(ctx, w.FormID, func(cur map[string]json.RawMessage, sys ebacState) (map[string]json.RawMessage, []string) {
		sys.Runtime = w.Runtime
		view := snap
		if snap != nil {
			set["prs"] = model.MustJSON(prsTree(snap))
			sys.Round, sys.TakenAt, sys.Complete, sys.Note = snap.Round, snap.TakenAt, snap.Complete, snap.Note
		} else if prs, err := prsFromTree(cur["prs"]); err == nil {
			view = &model.Snapshot{PRs: prs}
		}
		for k := range set {
			if strings.HasPrefix(k, protocolKeyPrefix) {
				delete(set, k)
			}
		}
		for i, c := range protocolChunks(protocolText(w, view, s.Me)) {
			set[fmt.Sprintf("%s%d", protocolKeyPrefix, i+1)] = model.MustJSON(c)
		}
		system := model.ObjectOf(cur["system"])
		system["ebac"] = model.MustJSON(sys)
		set["system"] = model.MustJSON(system)
		if fresh != nil {
			set["inbox"] = model.MustJSON(mergeInbox(cur["inbox"], fresh))
		}
		posts := model.ObjectOf(cur["post"])
		changed := false
		for id, fields := range receipts {
			if _, ok := posts[id]; !ok {
				continue // removed since it was read
			}
			p := model.ObjectOf(posts[id])
			for k, v := range fields {
				p[k] = v
			}
			posts[id] = model.MustJSON(p)
			changed = true
		}
		if prunePosts(posts, time.Now()) {
			changed = true
		}
		if changed {
			set["post"] = model.MustJSON(posts)
		}
		var remove []string
		for k := range cur {
			if k == "delta" || k == "sync" {
				remove = append(remove, k)
			}
			if strings.HasPrefix(k, protocolKeyPrefix) {
				if _, keep := set[k]; !keep {
					remove = append(remove, k)
				}
			}
		}
		return set, remove
	})
}

// criticConfig is a critic without the fields that live elsewhere.
func criticConfig(w *model.Critic) map[string]any {
	b, _ := json.Marshal(w)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "runtime")
	delete(m, "form_id")
	return m
}

// nextBoard applies whole top-level values and removals to a board.
func nextBoard(cur form.Snapshot, set map[string]json.RawMessage, remove []string) (form.Snapshot, error) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range append(keys, remove...) {
		if strings.Contains(k, ".") {
			return cur, fmt.Errorf("%q is not a top-level key", k)
		}
	}
	next := cur
	for _, k := range keys {
		next = next.SetPath(k, set[k])
	}
	for _, k := range remove {
		next = next.DeletePath(k)
	}
	return next, nil
}

// postKeep is how long a finished post stays on the form after its receipt.
const postKeep = time.Hour

// prunePosts drops finished posts whose receipt is older than postKeep, or
// that finished before receipts carried done_at. Unfinished posts stay.
func prunePosts(posts map[string]json.RawMessage, now time.Time) bool {
	pruned := false
	for id, raw := range posts {
		var p struct {
			model.Post
			DoneAt string `json:"done_at"`
		}
		if json.Unmarshal(raw, &p) != nil || !p.Post.Terminal() {
			continue
		}
		if t, err := time.Parse(time.RFC3339, p.DoneAt); err == nil && now.Sub(t) < postKeep {
			continue
		}
		delete(posts, id)
		pruned = true
	}
	return pruned
}

// FormStateBudget caps a form's whole state. figaro writes the entire state
// at the head of every log segment, so a form near the segment size blocks
// every write on the box (jack-work/figaro#44, 2026-10-21).
const FormStateBudget = 192 * 1024

// checkBudget refuses a write that grows the state past the budget. A write
// that shrinks it always passes, so an oversized form can still be compacted.
func checkBudget(id string, curBytes, nextBytes int) error {
	if nextBytes > FormStateBudget && nextBytes > curBytes {
		return fmt.Errorf("write %s refused: form state would be %d B, over the %d B budget "+
			"(figaro writes the whole state at every log segment head; jack-work/figaro#44)", id, nextBytes, FormStateBudget)
	}
	return nil
}

// commit diffs the top-level keys build returns against the live form and
// writes the difference as one versioned patch, re-reading on a lost race.
// Keys build does not name are left as the seat or another writer left them.
//
// Top-level only, always: form.SetPath through a member that is a leaf rather
// than an object adds a flat "a.b" sibling instead of nesting
// (jack-work/figaro#43); on 2026-10-20 that replaced one form's system subtree
// with null. Subtrees (system, post) are merged here and written whole.
func (s *Store) commit(ctx context.Context, id string, build func(cur map[string]json.RawMessage, sys ebacState) (map[string]json.RawMessage, []string)) error {
	c, err := s.Fig.Node(ctx, id)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(rand.Intn(attempt*2+1)) * time.Millisecond)
		}
		f, err := s.readForm(ctx, id)
		if err != nil {
			return err
		}
		set, remove := build(f.Keys, f.ebac())
		next, err := nextBoard(f.Snap, set, remove)
		if err != nil {
			return fmt.Errorf("write %s: %w", id, err)
		}
		curRaw, _ := json.Marshal(f.Snap)
		nextRaw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("write %s: %w", id, err)
		}
		if err := checkBudget(id, len(curRaw), len(nextRaw)); err != nil {
			return err
		}
		patch := next.Diff(f.Snap)
		if patch.IsIdentity() {
			return nil
		}
		if _, lastErr = c.Set(ctx, patch, f.Version); lastErr == nil {
			return nil
		}
		if !figaro.IsVersionConflict(lastErr) {
			return fmt.Errorf("write %s: %w", id, lastErr)
		}
	}
	return fmt.Errorf("write %s: %w", id, lastErr)
}

// Archive exports the critic and its PR state to disk, so the audit survives
// the form's lease, and marks it archived.
func (s *Store) Archive(ctx context.Context, w *model.Critic, snap *model.Snapshot) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(s.Root, "archive", w.Name+"-"+stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "critic.json"), w); err != nil {
		return "", err
	}
	if snap != nil {
		if err := writeJSONAtomic(filepath.Join(dir, "snapshot.json"), snap); err != nil {
			return "", err
		}
	}
	return dir, nil
}
