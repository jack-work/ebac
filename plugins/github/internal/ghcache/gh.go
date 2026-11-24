package ghcache

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GH is the entity form's gh key: GitHub's state of the PR or issue, as
// read. An issue form carries only the keys issueGH names (#45).
type GH struct {
	Kind         string            `json:"kind"` // pr | issue
	Number       int               `json:"number"`
	URL          string            `json:"url"`
	Title        string            `json:"title"`
	State        string            `json:"state"`
	Draft        bool              `json:"draft"`
	Author       string            `json:"author"` // lower case, as the board writes it
	Head         string            `json:"head"`
	Tree         string            `json:"tree,omitempty"`
	Base         string            `json:"base"`
	Branch       string            `json:"branch"`
	Labels       []string          `json:"labels,omitempty"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	ReadyAt      string            `json:"ready_at,omitempty"`
	Mergeable    string            `json:"mergeable"`
	MergeState   string            `json:"merge_state"`
	Lines        [2]int            `json:"lines"`
	Rollup       string            `json:"rollup,omitempty"`
	Checks       map[string]*Check `json:"checks"`
	Fixes        []int             `json:"fixes"` // closingIssuesReferences
	Reviews      map[string]RevAt  `json:"reviews,omitempty"`
	Threads      map[string]Thread `json:"threads,omitempty"`
	ThreadsTotal int               `json:"threads_total"`
	// Comments are the conversation comments by node id (the newest 30),
	// CommentsTotal how many there are.
	Comments      map[string]CachedComment `json:"comments,omitempty"`
	CommentsTotal int                      `json:"comments_total"`
	Body          Body                     `json:"body"`
}

// CachedComment is one conversation or review-thread comment as read. Cut:
// Body was trimmed to fit the form (not the whole text: it cannot be edited).
type CachedComment struct {
	Author    string `json:"author"` // lower case
	Body      string `json:"body"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at"`
	URL       string `json:"url,omitempty"`
	Cut       bool   `json:"cut,omitempty"`
}

// issueGH is an issue form's gh: GH without the PR-only keys.
type issueGH struct {
	Kind          string                   `json:"kind"`
	Number        int                      `json:"number"`
	URL           string                   `json:"url"`
	Title         string                   `json:"title"`
	State         string                   `json:"state"`
	Author        string                   `json:"author"`
	Labels        []string                 `json:"labels,omitempty"`
	CreatedAt     string                   `json:"created_at"`
	UpdatedAt     string                   `json:"updated_at"`
	Comments      map[string]CachedComment `json:"comments,omitempty"`
	CommentsTotal int                      `json:"comments_total"`
	Body          Body                     `json:"body"`
}

// wire is g as the form carries it.
func (g GH) wire() any {
	if g.Kind != KindIssue {
		return g
	}
	return issueGH{Kind: g.Kind, Number: g.Number, URL: g.URL, Title: g.Title, State: g.State, Author: g.Author, Labels: g.Labels,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Comments: g.Comments, CommentsTotal: g.CommentsTotal, Body: g.Body}
}

// Check is one pipeline: its state in the board's closed set (pass | fail |
// pending | cancelled | unknown), its run id and the failed jobs under it.
type Check struct {
	State      string   `json:"state"`
	RunID      int64    `json:"run_id,omitempty"`
	JobsFailed []string `json:"jobs_failed,omitempty"`
}

// RevAt is a login's newest decisive review: state and the sha it was on.
type RevAt struct {
	State string `json:"state"`
	Sha   string `json:"sha"`
	At    string `json:"at"`
}

// Thread is one review thread. Resolved is a write field (a seat sets it
// true to resolve); Replies are the seat's replies not yet posted (a seat
// appends {body}; the sync posts each once and drops it).
type Thread struct {
	Path     string  `json:"path"`
	Resolved bool    `json:"resolved"`
	LastBy   string  `json:"last_by"`
	N        int     `json:"n"`
	Replies  []Reply `json:"replies,omitempty"`
	// Comments are the thread's newest ThreadLast comments plus every one of
	// ours, by node id (#45).
	Comments map[string]CachedComment `json:"comments,omitempty"`
	// Partial: the thread has more comments than its whole read took (100);
	// ours past them are not on the form.
	Partial bool `json:"partial,omitempty"`
}

// Reply is one reply a seat wants posted on a thread.
type Reply struct {
	Body string `json:"body"`
}

// Body is the body: Text, the whole of it (#45: the in-place edit is
// gh.body.text), and its sections (split at H2; the preamble first, h ""),
// each a hash and its first SectionHead bytes. Cut: Text was trimmed to fit
// the form (not whole: it cannot be edited).
type Body struct {
	Bytes    int       `json:"bytes"`
	Hash     string    `json:"hash"`
	Text     string    `json:"text"`
	Cut      bool      `json:"cut,omitempty"`
	Sections []Section `json:"sections,omitempty"`
}

// Section is one H2 section of the body.
type Section struct {
	H    string `json:"h"`
	Hash string `json:"hash"`
	Head string `json:"head"`
}

// SectionHead caps a section's excerpt on the form.
const SectionHead = 300

// StateOf maps a CheckRun conclusion/status or a StatusContext state to the
// board's closed set (board-snapshot STATE_OF).
var StateOf = map[string]string{"SUCCESS": "pass", "NEUTRAL": "pass", "SKIPPED": "pass", "FAILURE": "fail", "TIMED_OUT": "fail",
	"STARTUP_FAILURE": "fail", "ACTION_REQUIRED": "fail", "ERROR": "fail", "CANCELLED": "cancelled", "STALE": "cancelled",
	"PENDING": "pending", "QUEUED": "pending", "IN_PROGRESS": "pending", "WAITING": "pending", "REQUESTED": "pending", "EXPECTED": "pending"}

var runID = regexp.MustCompile(`buildId=(\d+)`)

// ChecksOf groups check runs by pipeline (name without a " (job)" suffix):
// the pipeline-level run's state, else fail when a job failed, else
// pending (board-snapshot checks_of, every pipeline rather than Aether*
// only). A status context is its own pipeline.
func ChecksOf(cs []Context) map[string]*Check {
	out := map[string]*Check{}
	top := map[string]string{}
	for _, c := range cs {
		switch c.Typename {
		case "StatusContext":
			out[c.Context] = &Check{State: stateOf(c.State)}
			top[c.Context] = c.State
		case "CheckRun":
			name, _, _ := strings.Cut(c.Name, " (")
			p := out[name]
			if p == nil {
				p = &Check{}
				out[name] = p
			}
			if m := runID.FindStringSubmatch(c.DetailsURL); m != nil {
				p.RunID, _ = strconv.ParseInt(m[1], 10, 64)
			}
			if c.Name == name {
				s := c.Conclusion
				if s == "" {
					s = c.Status
				}
				top[name] = s
			} else if c.Conclusion == "FAILURE" || c.Conclusion == "TIMED_OUT" {
				job := strings.TrimSuffix(c.Name[len(name)+2:], ")")
				p.JobsFailed = append(p.JobsFailed, cut(job, 80))
			}
		}
	}
	for name, p := range out {
		if p.State != "" {
			continue
		}
		raw := top[name]
		if raw == "" {
			raw = "PENDING"
			if len(p.JobsFailed) > 0 {
				raw = "FAILURE"
			}
		}
		p.State = stateOf(raw)
		sort.Strings(p.JobsFailed)
	}
	return out
}

func stateOf(raw string) string {
	if s, ok := StateOf[raw]; ok {
		return s
	}
	return "unknown"
}

func hash12(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// Sections splits a body at lines starting "## ".
func Sections(body string) Body {
	b := Body{Bytes: len(body), Hash: hash12(body), Text: body}
	if body == "" {
		return b
	}
	var cur strings.Builder
	h := ""
	flush := func() {
		t := cur.String()
		if h != "" || strings.TrimSpace(t) != "" {
			b.Sections = append(b.Sections, Section{H: h, Hash: hash12(t), Head: cut(strings.TrimSpace(t), SectionHead)})
		}
		cur.Reset()
	}
	for _, l := range strings.SplitAfter(body, "\n") {
		if strings.HasPrefix(l, "## ") {
			flush()
			h = cut(strings.TrimSpace(l[3:]), 120)
			continue
		}
		cur.WriteString(l)
	}
	flush()
	return b
}

// GHOf builds gh from a deep read.
func GHOf(pr *PR) GH {
	g := GH{Kind: KindPR, Number: pr.Number, URL: pr.URL, Title: pr.Title, State: pr.State, Draft: pr.Draft,
		Author: strings.ToLower(login(pr.Author)), Head: pr.Head, Base: pr.Base, Branch: pr.Branch,
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, Mergeable: pr.Mergeable, MergeState: pr.MergeSt,
		Lines: [2]int{pr.Additions, pr.Deletions}, Checks: map[string]*Check{}, Fixes: []int{},
		ThreadsTotal: pr.Threads.TotalCount, Body: Sections(pr.Body)}
	for _, l := range pr.Labels.Nodes {
		g.Labels = append(g.Labels, l.Name)
	}
	sort.Strings(g.Labels)
	if n := pr.Ready.Nodes; len(n) > 0 {
		g.ReadyAt = n[len(n)-1].CreatedAt
	}
	if len(pr.Commits.Nodes) > 0 {
		c := pr.Commits.Nodes[0].Commit
		if c.Tree != nil {
			g.Tree = c.Tree.Oid
		}
		if c.Rollup != nil {
			g.Rollup = c.Rollup.State
			g.Checks = ChecksOf(c.Rollup.Contexts.Nodes)
		}
	}
	for _, n := range pr.Closing.Nodes {
		g.Fixes = append(g.Fixes, n.Number)
	}
	sort.Ints(g.Fixes)
	for _, r := range pr.Reviews.Nodes {
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		who := strings.ToLower(login(r.Author))
		if who == "" {
			who = "ghost" // a deleted account, as GitHub shows it
		}
		if prev, ok := g.Reviews[who]; ok && prev.At > r.SubmittedAt {
			continue
		}
		if g.Reviews == nil {
			g.Reviews = map[string]RevAt{}
		}
		sha := ""
		if r.Commit != nil {
			sha = r.Commit.Oid
		}
		g.Reviews[who] = RevAt{State: r.State, Sha: sha, At: r.SubmittedAt}
	}
	for _, t := range pr.Threads.Nodes {
		if g.Threads == nil {
			g.Threads = map[string]Thread{}
		}
		last := ""
		if n := t.Comments.Nodes; len(n) > 0 {
			last = login(n[len(n)-1].Author)
		}
		g.Threads[t.ID] = Thread{Path: t.Path, Resolved: t.Resolved, LastBy: last, N: t.Comments.TotalCount, Comments: cached(t.Comments.Nodes)}
	}
	g.Comments, g.CommentsTotal = cached(pr.Comments.Nodes), pr.Comments.TotalCount
	return g
}

// GHOfIssue builds an issue's gh from a deep read (#45).
func GHOfIssue(is *Issue) GH {
	g := GH{Kind: KindIssue, Number: is.Number, URL: is.URL, Title: is.Title, State: is.State,
		Author: strings.ToLower(login(is.Author)), CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		Comments: cached(is.Comments.Nodes), CommentsTotal: is.Comments.TotalCount, Body: Sections(is.Body)}
	for _, l := range is.Labels.Nodes {
		g.Labels = append(g.Labels, l.Name)
	}
	sort.Strings(g.Labels)
	return g
}

// cached keys comments by node id; one without an id is skipped (nothing
// could address it).
func cached(cs []Comment) map[string]CachedComment {
	var out map[string]CachedComment
	for _, c := range cs {
		if c.ID == "" {
			continue
		}
		if out == nil {
			out = map[string]CachedComment{}
		}
		out[c.ID] = CachedComment{Author: strings.ToLower(login(c.Author)), Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, URL: c.URL}
	}
	return out
}
