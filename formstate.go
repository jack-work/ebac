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

	"github.com/jack-work/figaro/api/form"
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
	Runtime  Runtime   `json:"runtime"`
	Round    int       `json:"snapshot_round,omitempty"`
	TakenAt  time.Time `json:"snapshot_taken_at,omitempty"`
	Complete bool      `json:"snapshot_complete,omitempty"`
	Note     string    `json:"snapshot_note,omitempty"`
}

// treePR is a PRState whose threads hold comments keyed by id: a list would
// make one new comment a rewrite of the whole thread.
type treePR struct {
	*PRState
	Threads map[string]*treeThread `json:"threads"`
}

type treeThread struct {
	*Thread
	Comments map[string]*Comment `json:"comments"`
}

// prFormKey makes a PR key safe as one path segment.
func prFormKey(k PRKey) string { return strings.ReplaceAll(string(k), ".", "%2E") }

func prsTree(s *Snapshot) map[string]*treePR {
	out := map[string]*treePR{}
	if s == nil {
		return out
	}
	for k, p := range s.PRs {
		cp := *p
		cp.Threads = nil
		t := &treePR{PRState: &cp, Threads: map[string]*treeThread{}}
		for id, th := range p.Threads {
			tc := *th
			tc.Comments = nil
			tt := &treeThread{Thread: &tc, Comments: map[string]*Comment{}}
			for _, c := range th.Comments {
				tt.Comments[c.ID] = c
			}
			t.Threads[id] = tt
		}
		out[prFormKey(k)] = t
	}
	return out
}

func prsFromTree(raw json.RawMessage) (map[PRKey]*PRState, error) {
	out := map[PRKey]*PRState{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var tree map[string]*treePR
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("prs: %w", err)
	}
	for _, t := range tree {
		if t == nil || t.PRState == nil {
			continue
		}
		p := t.PRState
		p.Threads = map[string]*Thread{}
		for id, tt := range t.Threads {
			if tt == nil || tt.Thread == nil {
				continue
			}
			th := tt.Thread
			th.Comments = make([]*Comment, 0, len(tt.Comments))
			for _, c := range tt.Comments {
				th.Comments = append(th.Comments, c)
			}
			sort.Slice(th.Comments, func(i, j int) bool {
				a, b := th.Comments[i], th.Comments[j]
				if a.CreatedAt != b.CreatedAt {
					return a.CreatedAt < b.CreatedAt
				}
				return a.ID < b.ID
			})
			p.Threads[id] = th
		}
		if p.Reviews == nil {
			p.Reviews = map[string]*Review{}
		}
		if p.Comments == nil {
			p.Comments = map[string]*Comment{}
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
	c, err := s.Fig.node(ctx, id)
	if err != nil {
		return nil, err
	}
	resp, err := c.Form(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", id, gone(err))
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

func (f *criticForm) critic() (*Critic, error) {
	raw, ok := f.Keys["critic"]
	if !ok {
		return nil, fmt.Errorf("form %s has no critic", f.ID)
	}
	var w Critic
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

func (f *criticForm) snapshot() (*Snapshot, error) {
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
	return &Snapshot{Critic: name, Round: e.Round, TakenAt: e.TakenAt, PRs: prs, Complete: e.Complete, Note: e.Note}, nil
}

// criticForms reads every form declaring kind ebac.critic.
func (s *Store) criticForms(ctx context.Context) ([]*criticForm, error) {
	ang, err := s.Fig.angelus()
	if err != nil {
		return nil, err
	}
	resp, err := ang.ListGlobal(ctx)
	if err != nil {
		return nil, fmt.Errorf("list forms: %w", err)
	}
	var out []*criticForm
	for _, r := range resp.Figaros {
		if r.Kind != "form" || !strings.HasPrefix(r.Name, "ebac-") {
			continue
		}
		f, err := s.readForm(ctx, r.ID)
		if errors.Is(err, ErrFormGone) {
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
		} else if !errors.Is(err, ErrFormGone) {
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

func (s *Store) LoadCritic(ctx context.Context, name string) (*Critic, error) {
	f, err := s.findCritic(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.critic()
}

func (s *Store) LoadSnapshot(ctx context.Context, name string) (*Snapshot, error) {
	f, err := s.findCritic(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.snapshot()
}

func (s *Store) ListCritics(ctx context.Context) ([]*Critic, error) {
	forms, err := s.criticForms(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Critic
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
func (s *Store) SaveCritic(ctx context.Context, w *Critic) error {
	return s.Save(ctx, w, nil, nil)
}

// Save writes the critic, and the snapshot when one is given, as ONE patch.
// extra carries further ebac-owned paths (outbox acks) in the same patch.
func (s *Store) Save(ctx context.Context, w *Critic, snap *Snapshot, extra map[string]json.RawMessage) error {
	if w.FormID == "" {
		return fmt.Errorf("critic %s has no form", w.Name)
	}
	cfg := *w
	cfg.Runtime = Runtime{}
	cfgRaw, err := json.Marshal(criticConfig(&cfg))
	if err != nil {
		return err
	}
	set := map[string]json.RawMessage{
		"kind":   mustJSON(criticKind),
		"critic": cfgRaw,
	}
	harness, err := json.Marshal(harnessContract(w))
	if err != nil {
		return err
	}
	set["harness"] = harness
	for k, v := range extra {
		set[k] = v
	}
	s.formOf[w.Name] = w.FormID
	return s.commit(ctx, w.FormID, func(cur map[string]json.RawMessage, sys ebacState) (map[string]json.RawMessage, []string) {
		sys.Runtime = w.Runtime
		view := snap
		if snap != nil {
			set["prs"] = mustJSON(prsTree(snap))
			sys.Round, sys.TakenAt, sys.Complete, sys.Note = snap.Round, snap.TakenAt, snap.Complete, snap.Note
		} else if prs, err := prsFromTree(cur["prs"]); err == nil {
			view = &Snapshot{PRs: prs}
		}
		for k := range set {
			if strings.HasPrefix(k, protocolKeyPrefix) {
				delete(set, k)
			}
		}
		for i, c := range protocolChunks(protocolText(w, view, s.Me)) {
			set[fmt.Sprintf("%s%d", protocolKeyPrefix, i+1)] = mustJSON(c)
		}
		set["system.ebac"] = mustJSON(sys)
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
func criticConfig(w *Critic) map[string]any {
	b, _ := json.Marshal(w)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "runtime")
	delete(m, "form_id")
	return m
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// commit diffs the paths build returns against the live form and writes the
// difference as one versioned patch, re-reading on a lost race. Keys build
// does not name are left as the seat or another writer left them.
func (s *Store) commit(ctx context.Context, id string, build func(cur map[string]json.RawMessage, sys ebacState) (map[string]json.RawMessage, []string)) error {
	c, err := s.Fig.node(ctx, id)
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
		next := f.Snap
		keys := make([]string, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next = next.SetPath(k, set[k])
		}
		for _, k := range remove {
			next = next.DeletePath(k)
		}
		patch := next.Diff(f.Snap)
		if patch.IsIdentity() {
			return nil
		}
		if _, lastErr = c.Set(ctx, patch, f.Version); lastErr == nil {
			return nil
		}
		if !isVersionConflict(lastErr) {
			return fmt.Errorf("write %s: %w", id, lastErr)
		}
	}
	return fmt.Errorf("write %s: %w", id, lastErr)
}

// Archive exports the critic and its PR state to disk, so the audit survives
// the form's lease, and marks it archived.
func (s *Store) Archive(ctx context.Context, w *Critic, snap *Snapshot) (string, error) {
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
