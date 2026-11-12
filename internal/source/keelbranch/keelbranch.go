// Package keelbranch is the keel-branches source (served by ebac-keel): one
// keel repo's work/<owner>/<topic> branches, ONE child form per branch
// (charter amendment 2, K2), holding the branch's facts: head, base, merge
// base, diff stat, changed files, commits, review threads (open and resolved,
// path+line@sha), and each reviewer's last verdict with the head it read.
//
// The branch's builder holds its form (target-aria) and owns code_ready and
// ready on it; keel CI owns pr_url and pr_number. The plugin never writes
// those (the core refuses them) and reads code_ready through the core.
//
// A pass reads the refs and the repo's threads (two calls when nothing moved),
// plus span and log for each branch whose head moved. It wakes a branch form's
// holder on a new comment in the branch's threads, and pings the Reviewer when
// code_ready turns true and on each new head once it is set. On its own role
// form it keeps the registry (key branches: branch -> child form) and the
// branches it could not follow (key overflow).
package keelbranch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/keel"
)

// Keel is what a pass needs from keel.
type Keel interface {
	Refs(ctx context.Context, repo string) ([]keel.Ref, error)
	Span(ctx context.Context, repo, base, head string) (*keel.Span, error)
	Log(ctx context.Context, repo, rev string, n int) ([]keel.Commit, error)
	// ThreadsAt places the repo's threads at a revision (a branch head).
	ThreadsAt(ctx context.Context, repo, rev string) (map[string][]keel.Thread, error)
	// Events is the repo's thread event feed after a sequence number.
	Events(ctx context.Context, repo string, after int64, wait time.Duration) ([]keel.FeedEvent, int64, error)
}

// Caps on what one branch form carries: a studied value is cut at 2046
// characters, so lists stay short and say how many more there are.
const (
	MaxFiles   = 40
	MaxCommits = 30
	MaxThreads = 40
	// MaxLog is how many of a branch's commits are read (and kept, 12 hex, to
	// tell its threads from other branches').
	MaxLog = 200
	// MaxPingsPerPass bounds one pass's pings (the core allows 10); the rest
	// wait in the state's queue (at most MaxQueue).
	MaxPingsPerPass = 8
	MaxQueue        = 50
	// SweepEvery: a branch whose threads were last read at least this many
	// passes ago may be read again by the sweep (a backstop for a missed
	// event), in reads the due branches leave over.
	SweepEvery = 10
	// MaxPaths bounds the changed paths a branch keeps to match feed events.
	MaxPaths = 300
	// MaxThreadReads bounds a pass's threads?rev reads of due branches (the
	// default of the critic's keel_branches.max_thread_reads): reads must not
	// stack past the plugin's timeout. Branches due beyond it wait (Due),
	// oldest first, for later passes. SweepReads bounds the sweep's reads in
	// the slots the due ones leave (keel_branches.sweep_reads).
	MaxThreadReads = 8
	SweepReads     = 2
)

// Branch is what the state keeps per followed branch.
type Branch struct {
	Key       string            `json:"k"`
	Head      string            `json:"h"`
	Facts     map[string]string `json:"f,omitempty"`  // fact key -> digest of the value last written
	Commits   []string          `json:"c,omitempty"`  // its commits since the merge base, 12 hex
	CodeReady bool              `json:"r,omitempty"`  // code_ready as last seen
	Seen      string            `json:"s,omitempty"`  // the newest comment time seen in its threads (RFC 3339 nano)
	Threads   []string          `json:"t,omitempty"`  // its threads' ids (feed events on them touch it)
	Paths     []string          `json:"p,omitempty"`  // its changed paths (a feed event on one touches it)
	Due       int               `json:"d,omitempty"`  // the pass its threads became due to be read (0: not due)
	Read      int               `json:"rd,omitempty"` // the pass its threads were last read
}

// State is the plugin's opaque state (critic runtime.keel_branches).
type State struct {
	Branches map[string]*Branch `json:"branches,omitempty"` // by ref name
	Registry string             `json:"registry,omitempty"` // digest of the branches key last written
	Overflow string             `json:"overflow,omitempty"` // digest of the overflow key last written
	Queue    []plugin.Ping      `json:"queue,omitempty"`
	// Feed is the keel event feed's cursor (FeedStarted once it is at the
	// feed's head); Passes counts passes for the sweep.
	Feed        int64 `json:"feed,omitempty"`
	FeedStarted bool  `json:"feed_started,omitempty"`
	Passes      int   `json:"passes,omitempty"`
}

// Pass is one round's inputs.
type Pass struct {
	Watch    model.KeelBranches
	Children map[string]string                     // child key -> @id
	Owned    map[string]map[string]json.RawMessage // child key -> its holder-owned keys
	Max      int                                   // children at most (model.MaxChildren)
	Now      time.Time
}

// Result is one round's outputs.
type Result struct {
	State State
	Forms []plugin.FormWrite
	Set   map[string]json.RawMessage
	Unset []string
	Pings []plugin.Ping
	Calls int
	Log   []string
}

// KeyOf is a branch's child key: <owner>--<topic>, lowercase, every other
// character a dash; ok false when the ref is not <prefix><owner>/<topic>.
func KeyOf(prefix, ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, prefix)
	if !ok {
		return "", false
	}
	owner, topic, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || topic == "" {
		return "", false
	}
	key := clean(owner) + "--" + clean(topic)
	if len(key) > 100 || key[0] == '-' {
		return "", false
	}
	return key, true
}

func clean(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// verdictRE is a reviewer's verdict marker in a keel comment:
// <!-- keel-review verdict:<approve|request_changes|comment> head:<sha> -->.
var verdictRE = regexp.MustCompile(`<!-- keel-review verdict:([A-Za-z_]+) head:([0-9a-f]{7,40}) -->`)

// Run is one pass.
func Run(ctx context.Context, k Keel, prev State, in Pass) (Result, error) {
	st := prev
	if st.Branches == nil {
		st.Branches = map[string]*Branch{}
	}
	res := Result{Set: map[string]json.RawMessage{}}
	logf := func(f string, a ...any) { res.Log = append(res.Log, fmt.Sprintf(f, a...)) }
	repo, base, prefix := in.Watch.Repo, in.Watch.Base, in.Watch.Prefix
	if base == "" {
		base = "main"
	}
	if prefix == "" {
		prefix = "work/"
	}
	max := in.Max
	if max <= 0 {
		max = model.MaxChildren
	}

	refs, err := k.Refs(ctx, repo)
	res.Calls++
	if err != nil {
		return res, fmt.Errorf("refs: %w", err)
	}
	baseFound := false
	type cand struct {
		ref keel.Ref
		key string
	}
	var cands []cand
	var overflow []string
	byKey := map[string]string{}
	for _, r := range refs {
		if r.Kind != "branch" {
			continue
		}
		if r.Name == base {
			baseFound = true
			continue
		}
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		key, ok := KeyOf(prefix, r.Name)
		if !ok {
			overflow = append(overflow, r.Name+": not "+prefix+"<owner>/<topic>")
			continue
		}
		cands = append(cands, cand{r, key})
	}
	if !baseFound {
		return res, fmt.Errorf("refs: no branch %s in %s", base, repo)
	}
	// Followed first, then the newest: a new branch never displaces a followed one.
	sort.SliceStable(cands, func(i, j int) bool {
		fi, fj := st.Branches[cands[i].ref.Name] != nil, st.Branches[cands[j].ref.Name] != nil
		if fi != fj {
			return fi
		}
		if cands[i].ref.Time != cands[j].ref.Time {
			return cands[i].ref.Time > cands[j].ref.Time
		}
		return cands[i].ref.Name < cands[j].ref.Name
	})
	live := map[string]keel.Ref{}
	keyOf := map[string]string{} // ref name -> child key
	for _, c := range cands {
		switch {
		case byKey[c.key] != "":
			overflow = append(overflow, fmt.Sprintf("%s: its form name collides with %s", c.ref.Name, byKey[c.key]))
		case len(live) >= max:
			overflow = append(overflow, fmt.Sprintf("%s: over the %d-form cap", c.ref.Name, max))
		default:
			byKey[c.key] = c.ref.Name
			keyOf[c.ref.Name] = c.key
			live[c.ref.Name] = c.ref
		}
	}
	sort.Strings(overflow)
	if len(overflow) > 0 {
		logf("NOT FOLLOWED (%d): %s", len(overflow), strings.Join(overflow, "; "))
	}

	// Which branches' threads to read this pass. Due: those a feed event touches (a thread of theirs, or a path they
	// change), those whose head moved, new ones; due ones are read first, longest due first. The sweep (a backstop
	// for a missed event) never makes a branch due: it only fills the reads left over, oldest read first, with a
	// branch last read at least SweepEvery passes ago, so it cannot grow a backlog the cap never drains.
	st.Passes++
	touchT, touchP := map[string]bool{}, map[string]bool{}
	switch {
	case !st.FeedStarted:
		head, err := feedHead(ctx, k, repo, &res)
		if err != nil {
			logf("feed: %v (started next pass)", err)
		} else {
			st.Feed, st.FeedStarted = head, true
		}
	default:
		evs, next, err := feedRead(ctx, k, repo, st.Feed, &res)
		if err != nil {
			logf("feed: %v (its cursor kept: the events are read next pass)", err)
			break
		}
		st.Feed = next
		for _, e := range evs {
			touchT[e.Thread], touchP[e.Path] = true, true
		}
	}

	var pings []plugin.Ping
	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	sort.Strings(names)

	// Due: what was due and not read yet, plus what this pass makes due. Read: the maxReads longest due.
	maxReads, sweepReads := in.Watch.MaxThreadReads, in.Watch.SweepReads
	if maxReads <= 0 {
		maxReads = MaxThreadReads
	}
	if sweepReads <= 0 {
		sweepReads = SweepReads
	}
	type due struct {
		name  string
		since int
	}
	var queue []due
	dueNow := map[string]bool{}
	for _, name := range names {
		b := st.Branches[name]
		now := b == nil || live[name].SHA != b.Head || in.Children[b.Key] == "" || touched(b, touchT, touchP)
		dueNow[name] = now
		switch {
		case b != nil && b.Due > 0:
			queue = append(queue, due{name, b.Due})
		case now:
			queue = append(queue, due{name, st.Passes})
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].since != queue[j].since {
			return queue[i].since < queue[j].since
		}
		return queue[i].name < queue[j].name
	})
	readNow := map[string]bool{}
	for i, q := range queue {
		if i == maxReads {
			logf("threads of %d branch(es) deferred to later passes (at most %d reads a pass)", len(queue)-i, maxReads)
			break
		}
		readNow[q.name] = true
	}
	var sweep []string
	for _, name := range names {
		if b := st.Branches[name]; b != nil && !readNow[name] && b.Due == 0 && !dueNow[name] && st.Passes-b.Read >= SweepEvery {
			sweep = append(sweep, name)
		}
	}
	sort.SliceStable(sweep, func(i, j int) bool { return st.Branches[sweep[i]].Read < st.Branches[sweep[j]].Read })
	swept := 0
	for _, name := range sweep {
		if len(readNow) == maxReads || swept == sweepReads {
			break
		}
		readNow[name] = true
		swept++
	}

	for _, name := range names {
		ref := live[name]
		b := st.Branches[name]
		fresh := b == nil
		if fresh {
			b = &Branch{Key: keyOf[name], Facts: map[string]string{}}
			st.Branches[name] = b
		}
		if b.Facts == nil || !fresh && in.Children[b.Key] == "" {
			b.Facts = map[string]string{} // its form was never minted (or was lost): write every fact again
		}
		facts := map[string]any{}
		moved := ref.SHA != b.Head || len(b.Facts) == 0 // a form to (re)write whole reads its span again
		if moved {
			sp, err := k.Span(ctx, repo, base, ref.SHA)
			res.Calls++
			if err != nil {
				return res, fmt.Errorf("span %s: %w", name, err)
			}
			n := min(*sp.Count, MaxLog)
			var log []keel.Commit
			if n > 0 {
				log, err = k.Log(ctx, repo, ref.SHA, n)
				res.Calls++
				if err != nil {
					return res, fmt.Errorf("log %s: %w", name, err)
				}
			}
			if len(log) > n {
				log = log[:n]
			}
			b.Commits = b.Commits[:0]
			for _, c := range log {
				b.Commits = append(b.Commits, short(c.SHA, 12))
			}
			facts["head"] = ref.SHA
			facts["base"] = base
			facts["merge_base"] = sp.Base.SHA
			facts["diffstat"], facts["files"] = filesOf(sp.Files)
			facts["commits"] = commitsOf(log, *sp.Count)
			b.Paths = pathsOf(sp.Files)
		}
		facts["branch"] = name
		if (dueNow[name] || moved) && b.Due == 0 {
			b.Due = st.Passes
		}
		var mine []keel.Thread
		if readNow[name] {
			threads, err := k.ThreadsAt(ctx, repo, ref.SHA)
			res.Calls++
			if err != nil {
				// One branch's threads not read (a cold keel blaming past the timeout) fails nothing else: its
				// last known threads stand, and it stays due for a later pass.
				logf("threads of %s at %s: %v (kept the last known, retried later)", name, short(ref.SHA, 8), err)
				threads = nil
			}
			if err == nil {
				b.Due, b.Read = 0, st.Passes
				mine = branchThreads(threads, name, b.Commits)
				facts["threads"] = threadsOf(mine)
				facts["reviewers"] = reviewersOf(mine)
				b.Threads = b.Threads[:0]
				for _, t := range mine {
					b.Threads = append(b.Threads, t.ID)
				}
			}
		}

		// Wake the holder on comments newer than the last pass's (a new branch is a baseline).
		newest, news := b.Seen, []string{}
		for _, t := range mine {
			for _, c := range t.Comments {
				at := c.Created.UTC().Format(time.RFC3339Nano)
				if at > newest {
					newest = at
				}
				if !fresh && at > b.Seen {
					news = append(news, fmt.Sprintf("%s by %s", where(t), who(c)))
				}
			}
		}
		b.Seen = newest
		if len(news) > 0 {
			if id := in.Children[b.Key]; id != "" {
				sort.Strings(news)
				pings = append(pings, plugin.Ping{To: id, Text: oneLine(fmt.Sprintf("keel-branches: %s: %d new comment(s): %s",
					name, len(news), strings.Join(news, "; ")))})
			}
		}

		// The Reviewer: when code_ready turns true, and on each new head once it is set.
		ready := truthy(in.Owned[b.Key]["code_ready"])
		if r := in.Watch.Reviewer; r != "" && ready {
			form := in.Children[b.Key]
			switch {
			case !b.CodeReady:
				pings = append(pings, plugin.Ping{To: r, Text: oneLine(fmt.Sprintf("keel-branches: %s code_ready at %s; form %s; keel %s span %s...%s",
					name, short(ref.SHA, 8), form, repo, base, name))})
			case moved:
				pings = append(pings, plugin.Ping{To: r, Text: oneLine(fmt.Sprintf("keel-branches: %s new head %s (code_ready); form %s; keel %s span %s...%s",
					name, short(ref.SHA, 8), form, repo, base, name))})
			}
		}
		b.CodeReady = ready
		b.Head = ref.SHA

		set := map[string]json.RawMessage{}
		for k, v := range facts {
			raw := model.MustJSON(v)
			if d := digest(raw); d != b.Facts[k] {
				set[k] = raw
				b.Facts[k] = d
			}
		}
		if len(set) > 0 {
			res.Forms = append(res.Forms, plugin.FormWrite{Key: b.Key, Set: set})
		}
	}

	// A branch gone from keel (merged and deleted, or dropped): its form retires (archive rename; its lease lapses).
	for _, name := range sortedNames(st.Branches) {
		if _, ok := live[name]; ok {
			continue
		}
		b := st.Branches[name]
		res.Forms = append(res.Forms, plugin.FormWrite{Key: b.Key, Retire: true})
		delete(st.Branches, name)
		logf("%s: gone from keel, form %s retired", name, in.Children[b.Key])
	}

	// The registry: every followed branch whose form exists (a form minted this pass joins it next pass).
	reg := map[string]string{}
	for name, b := range st.Branches {
		if id := in.Children[b.Key]; id != "" {
			reg[name] = id
		}
	}
	if d := digest(model.MustJSON(reg)); d != st.Registry {
		res.Set["branches"] = model.MustJSON(reg)
		st.Registry = d
	}
	if d := digest(model.MustJSON(overflow)); d != st.Overflow {
		if len(overflow) == 0 {
			res.Unset = append(res.Unset, "overflow")
		} else {
			res.Set["overflow"] = model.MustJSON(overflow)
		}
		st.Overflow = d
	}

	res.Pings = queuePings(&st, pings, logf)
	res.State = st
	return res, nil
}

// branchThreads are the threads started on the branch: anchored at one of its
// commits, or on a span whose head is the branch.
func branchThreads(all map[string][]keel.Thread, name string, commits []string) []keel.Thread {
	var out []keel.Thread
	for _, ts := range all {
		for _, t := range ts {
			if mineThread(t, name, commits) {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mineThread(t keel.Thread, name string, commits []string) bool {
	if sp := t.Origin.Span; sp != "" {
		head := sp
		if i := strings.LastIndex(sp, ".."); i >= 0 {
			head = strings.TrimPrefix(sp[i+2:], ".")
		}
		if head == name {
			return true
		}
	}
	rev := short(t.Origin.Rev, 12)
	if len(rev) < 7 {
		return false
	}
	for _, c := range commits {
		if strings.HasPrefix(c, rev) || strings.HasPrefix(rev, c) {
			return true
		}
	}
	return false
}

// Thread is one review thread on a branch form.
type Thread struct {
	ID       string `json:"id"`
	At       string `json:"at"` // path:line@sha8, where it was anchored
	Resolved bool   `json:"resolved"`
	Comments int    `json:"comments"`
	Last     string `json:"last"` // the last comment's author
}

// Threads is a branch's review threads, open ones first.
type Threads struct {
	Open     int      `json:"open"`
	Resolved int      `json:"resolved"`
	List     []Thread `json:"list"`
	More     int      `json:"more,omitempty"`
}

func threadsOf(ts []keel.Thread) Threads {
	out := Threads{List: []Thread{}}
	sorted := append([]keel.Thread(nil), ts...)
	sort.SliceStable(sorted, func(i, j int) bool { return !sorted[i].Resolved && sorted[j].Resolved })
	for _, t := range sorted {
		if t.Resolved {
			out.Resolved++
		} else {
			out.Open++
		}
		if len(out.List) == MaxThreads {
			out.More++
			continue
		}
		e := Thread{ID: t.ID, At: where(t), Resolved: t.Resolved, Comments: len(t.Comments)}
		if n := len(t.Comments); n > 0 {
			e.Last = who(t.Comments[n-1])
		}
		out.List = append(out.List, e)
	}
	return out
}

// where is a thread's anchor, path:line@sha8.
func where(t keel.Thread) string {
	p, l := "?", 0
	if t.Anchor != nil {
		p, l = t.Anchor.File, t.Anchor.StartLine
	}
	return fmt.Sprintf("%s:%d@%s", p, l, short(t.Origin.Rev, 8))
}

func who(c keel.Comment) string {
	switch {
	case c.Alias != "":
		return c.Alias
	case c.Account != "":
		return c.Account
	}
	return c.Author
}

// Verdict is a reviewer's last verdict and the head it read.
type Verdict struct {
	Verdict  string `json:"verdict"`
	HeadRead string `json:"head_read"`
	At       string `json:"at"`
}

func reviewersOf(ts []keel.Thread) map[string]Verdict {
	out := map[string]Verdict{}
	for _, t := range ts {
		for _, c := range t.Comments {
			m := verdictRE.FindStringSubmatch(c.Body)
			if m == nil {
				continue
			}
			at := c.Created.UTC().Format(time.RFC3339Nano)
			if v, ok := out[who(c)]; ok && v.At >= at {
				continue
			}
			out[who(c)] = Verdict{Verdict: strings.ToUpper(m[1]), HeadRead: m[2], At: at}
		}
	}
	return out
}

// DiffStat is a span's size.
type DiffStat struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Files are the changed files ("<status> <path> +a -d"), the first MaxFiles.
type Files struct {
	List []string `json:"list"`
	More int      `json:"more,omitempty"`
}

func filesOf(cs []keel.Change) (DiffStat, Files) {
	ds := DiffStat{Files: len(cs)}
	fs := Files{List: []string{}}
	for _, c := range cs {
		a, d := "bin", ""
		if c.Additions != nil && c.Deletions != nil {
			ds.Additions += *c.Additions
			ds.Deletions += *c.Deletions
			a, d = fmt.Sprintf("+%d", *c.Additions), fmt.Sprintf(" -%d", *c.Deletions)
		}
		if len(fs.List) == MaxFiles {
			fs.More++
			continue
		}
		p := c.Path
		if c.RenamedFrom != "" {
			p = c.RenamedFrom + " -> " + p
		}
		fs.List = append(fs.List, fmt.Sprintf("%s %s %s%s", c.Status, p, a, d))
	}
	return ds, fs
}

// Commits are the branch's commits since the merge base, newest first, the
// first MaxCommits ("<sha8> <subject>").
type Commits struct {
	Count int      `json:"count"`
	List  []string `json:"list"`
}

func commitsOf(log []keel.Commit, count int) Commits {
	out := Commits{Count: count, List: []string{}}
	for _, c := range log {
		if len(out.List) == MaxCommits {
			break
		}
		s := c.Subject
		if len(s) > 72 {
			s = s[:71] + "~"
		}
		out.List = append(out.List, short(c.SHA, 8)+" "+s)
	}
	return out
}

func truthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		x = strings.ToLower(strings.TrimSpace(x))
		return x != "" && x != "false" && x != "0" && x != "no"
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}

// queuePings sends at most MaxPingsPerPass this pass, oldest first; the rest
// wait (bounded at MaxQueue, the oldest dropped and said).
func queuePings(st *State, fresh []plugin.Ping, logf func(string, ...any)) []plugin.Ping {
	q := append(st.Queue, fresh...)
	if over := len(q) - MaxQueue; over > 0 {
		logf("ping queue full: %d oldest dropped", over)
		q = q[over:]
	}
	n := min(len(q), MaxPingsPerPass)
	send := append([]plugin.Ping(nil), q[:n]...)
	st.Queue = nil
	if n < len(q) {
		st.Queue = append([]plugin.Ping(nil), q[n:]...)
	}
	return send
}

func sortedNames(m map[string]*Branch) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:399] + "~"
	}
	return s
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// pathsOf are a span's changed paths (a renamed file under both names), at most MaxPaths.
func pathsOf(cs []keel.Change) []string {
	var out []string
	for _, c := range cs {
		for _, p := range []string{c.Path, c.RenamedFrom} {
			if p != "" && len(out) < MaxPaths {
				out = append(out, p)
			}
		}
	}
	return out
}

// touched says whether a feed event names one of the branch's threads or a path it changes.
func touched(b *Branch, threads, paths map[string]bool) bool {
	for _, t := range b.Threads {
		if threads[t] {
			return true
		}
	}
	for _, p := range b.Paths {
		if paths[p] {
			return true
		}
	}
	return false
}

// feedRead reads the feed after a cursor to its end (one call when nothing happened).
func feedRead(ctx context.Context, k Keel, repo string, after int64, res *Result) ([]keel.FeedEvent, int64, error) {
	var all []keel.FeedEvent
	for page := 0; page < 20; page++ {
		evs, next, err := k.Events(ctx, repo, after, 0)
		res.Calls++
		if err != nil {
			return nil, after, err
		}
		if len(evs) == 0 {
			return all, next, nil
		}
		all, after = append(all, evs...), next
	}
	return all, after, nil
}

// feedHead is the feed's current end: where a first pass starts following it.
func feedHead(ctx context.Context, k Keel, repo string, res *Result) (int64, error) {
	_, next, err := feedRead(ctx, k, repo, 0, res)
	return next, err
}
