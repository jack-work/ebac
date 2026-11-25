package ghcache

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gluck/ebac/sdk/disclose"
	"github.com/gluck/ebac/sdk/forge"
	"github.com/gluck/ebac/sdk/model"
)

// comment_edit (#64; the augustine contract, section 2): a seat edits the
// body of one of the account's own comments IN PLACE on the entity form
// (gh.comments.<id>.body, gh.threads.<t>.comments.<id>.body), and the sync
// diffs it three ways like the wave-1 fields: form at base -> GitHub's
// stands; form moved and GitHub at base -> the edit is written after a fresh
// read of that one comment shows GitHub's body still at the base and
// viewerDidAuthor (the account wrote it); both moved -> a conflict alarm, the
// form's text in sync.conflicts. Another author's comment, a comment cut to
// fit the form, a body the disclosure rules refuse, or a field outside the
// trusted write_fields is a write-refused alarm, and nothing is written.

// allComments are the entity's cached comments, conversation and review
// threads together, by node id.
func allComments(g *GH) map[string]CachedComment {
	out := map[string]CachedComment{}
	for id, c := range g.Comments {
		out[id] = c
	}
	for _, t := range g.Threads {
		for id, c := range t.Comments {
			out[id] = c
		}
	}
	return out
}

func noBodies(cs map[string]CachedComment) map[string]CachedComment {
	if cs == nil {
		return nil
	}
	out := make(map[string]CachedComment, len(cs))
	for id, c := range cs {
		c.Body = ""
		out[id] = c
	}
	return out
}

// setComment sets comment id's body on gh (wherever it lives), copying the
// maps it touches so G is never changed through a shared map.
func setComment(g *GH, id, body string) {
	if c, ok := g.Comments[id]; ok {
		m := make(map[string]CachedComment, len(g.Comments))
		for k, v := range g.Comments {
			m[k] = v
		}
		c.Body = body
		m[id] = c
		g.Comments = m
		return
	}
	for tid, t := range g.Threads {
		c, ok := t.Comments[id]
		if !ok {
			continue
		}
		m := make(map[string]CachedComment, len(t.Comments))
		for k, v := range t.Comments {
			m[k] = v
		}
		c.Body = body
		m[id] = c
		t.Comments = m
		threads := make(map[string]Thread, len(g.Threads))
		for k, v := range g.Threads {
			threads[k] = v
		}
		threads[tid] = t
		g.Threads = threads
		return
	}
}

// editWant is one comment edit to write.
type editWant struct {
	id, body, base string // base: GitHub's body as read, equal to the agreed base
}

func commentField(id string) string { return "comment-" + id }

// commentEdits decides and writes the comment edits of one entity.
func (s *syncer) commentEdits(k string, e *Entity, G GH, F *GH, B Base, out *GH, next *Base, auth func(string) string) {
	fc, gc := allComments(F), allComments(&G)
	ids := make([]string, 0, len(fc))
	for id := range fc {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ours := strings.ToLower(s.p.Ours)
	keepForm := func(id, body string) { // the wish waits: the form keeps its text against the old base
		setComment(out, id, body)
		if h, ok := B.Comments[id]; ok {
			if next.Comments == nil {
				next.Comments = map[string]string{}
			}
			next.Comments[id] = h
		}
	}
	var wants []editWant
	for _, id := range ids {
		f := fc[id]
		bh, hadBase := B.Comments[id]
		g, onGH := gc[id]
		if f.Cut || (onGH && g.Cut) {
			// A body cut to fit the form is never an edit: the form holds a
			// head of the text, the base hashes the whole (live 11-25: every
			// cut comment read as an edit and raised a write-refused alarm).
			// The alarms that misreading raised are cleared here.
			delete(e.Alarms, AlarmWriteRefused+"."+commentField(id))
			continue
		}
		if !hadBase {
			continue // first seen this round: nothing to diff yet (the base is GitHub's)
		}
		if hash12(f.Body) == bh {
			continue // the form is at the base: GitHub's stands
		}
		field := commentField(id)
		switch {
		case !nodeIDRe.MatchString(id):
			s.alarm(k, e, AlarmUnparseable, field, fmt.Sprintf("comment id %q is not a node id", cut(id, 40)), nil, id, nil)
		case !onGH:
			s.alarm(k, e, AlarmWriteRefused, field, "the comment is not on the entity as read (deleted, or past the comments read): not edited (in sync.conflicts)", bh, f.Body, nil)
			s.conflictValue(e, field, f.Body)
		case ours == "" || strings.ToLower(g.Author) != ours:
			s.alarm(k, e, AlarmWriteRefused, field, fmt.Sprintf("the comment is %s's, not ours: another author's text is never edited", model.FirstNonEmpty(g.Author, "an unknown author")), bh, f.Body, g.Body)
		case f.Body == g.Body:
			settled(e, field) // GitHub already has the form's text
		case hash12(g.Body) != bh:
			s.alarm(k, e, AlarmConflict, field, "the comment was edited on GitHub since the base and on the form: GitHub's stands, the form's is in sync.conflicts", bh, f.Body, g.Body)
			s.conflictValue(e, field, f.Body)
		case strings.TrimSpace(f.Body) == "":
			s.alarm(k, e, AlarmUnparseable, field, "an empty comment body: not written", bh, f.Body, g.Body)
		default:
			if why := auth(model.FieldCommentEdit); why != "" {
				s.alarm(k, e, AlarmWriteRefused, field, why, bh, f.Body, g.Body)
			} else if why := disclose.Disclosure(f.Body); why != "" {
				s.alarm(k, e, AlarmWriteRefused, field, why, bh, f.Body, g.Body)
				s.conflictValue(e, field, f.Body)
			} else {
				wants = append(wants, editWant{id: id, body: f.Body, base: g.Body})
			}
		}
	}
	if len(wants) == 0 {
		return
	}
	if room := MaxWrites - s.writes; len(wants) > room {
		room = max(room, 0)
		s.logf("%s: %d comment edit(s) wait for the next round (cap %d per round)", k, len(wants)-room, MaxWrites)
		for _, w := range wants[room:] {
			keepForm(w.id, w.body)
		}
		if wants = wants[:room]; len(wants) == 0 {
			return
		}
	}
	fresh, err := s.readComments(wants)
	if err != nil {
		s.logf("%s: re-read of the comments before the edit failed: %v (the edits wait)", k, err)
		for _, w := range wants {
			keepForm(w.id, w.body)
		}
		return
	}
	for _, w := range wants {
		field := commentField(w.id)
		c := fresh[w.id]
		switch {
		case c == nil:
			s.alarm(k, e, AlarmWriteRefused, field, "the comment is gone from GitHub: not edited (in sync.conflicts)", nil, w.body, nil)
			s.conflictValue(e, field, w.body)
		case c.Number != e.N:
			s.alarm(k, e, AlarmWriteRefused, field, fmt.Sprintf("the comment belongs to #%d, not #%d: not edited", c.Number, e.N), nil, w.body, nil)
		case !c.Mine:
			s.alarm(k, e, AlarmWriteRefused, field, "GitHub says the account did not write this comment: another author's text is never edited", nil, w.body, nil)
		case c.Body == w.body:
			s.pushComment(e, out, next, w.id, w.body) // already there (a round that never saved)
			settled(e, field)
		case c.Body != w.base:
			if retry(e, field) {
				s.alarm(k, e, AlarmConflict, field, "the comment moved on GitHub between the read and the write, twice: GitHub's stands, the form's is in sync.conflicts", nil, w.body, c.Body)
				s.conflictValue(e, field, w.body)
			} else {
				s.logf("%s: comment %s: GitHub moved since the read (stale base): not written, retried next round", k, w.id)
				keepForm(w.id, w.body)
			}
		default:
			if _, err := s.mutate(forge.EditCommentMutation(c.Review), map[string]string{"c": w.id, "b": w.body}); err != nil {
				if refusedByGitHub(err) || retry(e, field) {
					s.alarm(k, e, AlarmWriteRefused, field, "GitHub refused the comment edit: "+err.Error(), nil, w.body, nil)
					s.conflictValue(e, field, w.body)
				} else {
					s.logf("%s: comment %s edit failed: %v (retried next round)", k, w.id, err)
					keepForm(w.id, w.body)
				}
				continue
			}
			s.pushComment(e, out, next, w.id, w.body)
			settled(e, field)
			s.logf("%s: edited comment %s", k, w.id)
		}
	}
}

// pushComment records a body GitHub now holds: on the gh written, in the
// base, and in the round's pushed fields (it is the base whether or not the
// form write lands).
func (s *syncer) pushComment(e *Entity, out *GH, next *Base, id, body string) {
	setComment(out, id, body)
	if next.Comments == nil {
		next.Comments = map[string]string{}
	}
	next.Comments[id] = hash12(body)
	p := e.pushed()
	if p.Comments == nil {
		p.Comments = map[string]string{}
	}
	p.Comments[id] = hash12(body)
}

// freshComment is one comment as GitHub has it right before an edit.
type freshComment struct {
	Body   string
	Mine   bool // viewerDidAuthor
	Number int  // the issue or pull request it is on
	Review bool // a pull request review comment (else an issue/PR conversation comment)
}

func commentReadQuery(ws []editWant) string {
	var b strings.Builder
	b.WriteString("{")
	for i, w := range ws {
		fmt.Fprintf(&b, ` c%d: node(id:"%s"){__typename ... on IssueComment{body viewerDidAuthor issue{number}} ... on PullRequestReviewComment{body viewerDidAuthor pullRequest{number}}}`, i, w.id)
	}
	b.WriteString("}")
	return b.String()
}

// readComments reads the comments to edit fresh; a comment GitHub no
// longer has is absent from the map.
func (s *syncer) readComments(ws []editWant) (map[string]*freshComment, error) {
	data, errs, err := s.gh.GraphQLPartial(s.ctx, commentReadQuery(ws))
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		if len(e.Path) == 1 && e.Type == "NOT_FOUND" {
			continue // a deleted comment: a null node
		}
		return nil, fmt.Errorf("%s %s at %s", e.Type, e.Message, strings.Join(e.Path, "."))
	}
	var raw map[string]*struct {
		Type   string                `json:"__typename"`
		Body   string                `json:"body"`
		Mine   bool                  `json:"viewerDidAuthor"`
		Issue  *struct{ Number int } `json:"issue"`
		PullRq *struct{ Number int } `json:"pullRequest"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := map[string]*freshComment{}
	for i, w := range ws {
		c := raw[fmt.Sprintf("c%d", i)]
		if c == nil || (c.Type != "IssueComment" && c.Type != "PullRequestReviewComment") {
			continue
		}
		fc := &freshComment{Body: c.Body, Mine: c.Mine, Review: c.Type == "PullRequestReviewComment"}
		if c.Issue != nil {
			fc.Number = c.Issue.Number
		}
		if c.PullRq != nil {
			fc.Number = c.PullRq.Number
		}
		out[w.id] = fc
	}
	return out, nil
}
