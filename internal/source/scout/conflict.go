package scout

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Base-head watch (Minister 11-12): a merge into a PR's base branch does not bump the PR's updatedAt, so the
// incremental search never re-reads it, and the conflict that merge caused went unseen (#108680, 11-12). Each pass
// reads the head of every distinct base ref of the tracked open PRs (one aliased query, refs only). For the PRs whose
// base head moved, and those still recomputing from an earlier move, it reads only mergeable + mergeStateStatus
// (aliased, LightPage a query). A PR whose settled mergeability changed joins the full re-read, which rewrites its
// entry and pings its owner on a turn into conflict; UNKNOWN (GitHub recomputing) is read again next pass and is
// never a conflict. Nothing moved: no mergeability read.

// LightPage is how many PRs one mergeability query asks (two scalar fields each).
const LightPage = 100

// withRefs adds to a root query (the search's first page) an alias bh reading the head commit of each named branch
// (r<i>): the base-head watch costs no call of its own.
func withRefs(q, repo string, refs []string) string {
	if len(refs) == 0 || !strings.HasSuffix(q, "}") {
		return q
	}
	owner, name, _ := strings.Cut(repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "bh: repository(owner:%q, name:%q){", owner, name)
	for i, r := range refs {
		fmt.Fprintf(&b, "r%d: ref(qualifiedName:%q){target{oid}} ", i, "refs/heads/"+r)
	}
	b.WriteString("} ")
	return q[:len(q)-1] + b.String() + "}"
}

// openBases is the sorted distinct base branches of the tracked open PRs.
func openBases(st *State) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range st.PRs {
		if b := baseOf(t); t.State == "OPEN" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// decodeRefs reads the bh alias: branch -> head (8 hex); a branch GitHub no longer has is absent. nil when the
// answer carries no bh (refs were not asked).
func decodeRefs(data json.RawMessage, refs []string) (map[string]string, error) {
	var r struct {
		Repository map[string]*struct {
			Target struct {
				Oid string `json:"oid"`
			} `json:"target"`
		} `json:"bh"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode refs: %w", err)
	}
	if r.Repository == nil {
		return nil, nil
	}
	out := map[string]string{}
	for i, ref := range refs {
		if x := r.Repository[fmt.Sprintf("r%d", i)]; x != nil && x.Target.Oid != "" {
			out[ref] = cut(x.Target.Oid, 8)
		}
	}
	return out, nil
}

// mergeQuery reads only the mergeability of named PRs (aliases p<n>).
func mergeQuery(repo string, prs []int) string {
	owner, name, _ := strings.Cut(repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "{repository(owner:%q, name:%q){", owner, name)
	for _, n := range prs {
		fmt.Fprintf(&b, "p%d: pullRequest(number:%d){number mergeable mergeStateStatus} ", n, n)
	}
	b.WriteString("}}")
	return b.String()
}

type mergeFacts struct {
	Number           int    `json:"number"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
}

func decodeMerge(data json.RawMessage) (map[int]mergeFacts, error) {
	var r struct {
		Repository map[string]*mergeFacts `json:"repository"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode mergeability: %w", err)
	}
	out := map[int]mergeFacts{}
	for _, m := range r.Repository {
		if m != nil && m.Number != 0 {
			out[m.Number] = *m
		}
	}
	return out, nil
}

// baseOf is the base a TrackedPR keeps ("" is main).
func baseOf(t *TrackedPR) string {
	if t.Base == "" {
		return "main"
	}
	return t.Base
}

// settledConflict is true for a settled conflict: mergeable CONFLICTING or merge state DIRTY. UNKNOWN never is.
func settledConflict(mergeable, mergeState string) bool {
	return mergeable == "CONFLICTING" || mergeState == "DIRTY"
}

// watchBases takes the base heads the search read (refs, heads; heads nil when not read: nothing moves) and reads the
// mergeability of the PRs whose base moved, and of those still recomputing. It returns the tracked open PRs (not in
// skip) whose settled mergeability changed: the caller re-reads them in full.
func watchBases(st *State, repo string, refs []string, heads map[string]string, skip map[int]bool,
	call func(string) (json.RawMessage, error), logf func(string, ...any)) ([]int, error) {
	if len(refs) == 0 {
		st.Bases, st.Recheck = nil, nil
		return nil, nil
	}
	moved := map[string]bool{}
	for _, r := range refs {
		h, ok := heads[r]
		if ok && h != st.Bases[r] {
			moved[r] = true // a first sighting counts: its PRs' mergeability is read once
		}
	}
	if heads != nil {
		st.Bases = heads
	}

	want := map[int]bool{}
	for _, n := range st.Recheck {
		want[n] = true
	}
	for n, t := range st.PRs {
		if t.State == "OPEN" && moved[baseOf(t)] {
			want[n] = true
		}
	}
	var ask []int
	for n := range want {
		if t := st.PRs[n]; t == nil || t.State != "OPEN" {
			continue
		}
		if skip[n] {
			continue // read in full this pass anyway
		}
		ask = append(ask, n)
	}
	sort.Ints(ask)
	if len(moved) > 0 {
		names := make([]string, 0, len(moved))
		for r := range moved {
			names = append(names, r)
		}
		sort.Strings(names)
		logf("base head moved: %s: mergeability of %d PR(s) read", strings.Join(names, ", "), len(ask))
	}

	var changed, recheck []int
	chunk := LightPage
	for i := 0; i < len(ask); {
		part := ask[i:min(i+chunk, len(ask))]
		data, err := call(mergeQuery(repo, part))
		if err != nil && tooHeavy(err) && chunk/2 >= MinPage {
			chunk /= 2
			logf("mergeability batch too heavy for GitHub: %d PRs a batch", chunk)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("mergeability: %w", err)
		}
		i += len(part)
		got, err := decodeMerge(data)
		if err != nil {
			return nil, err
		}
		for _, n := range part {
			m, ok := got[n]
			switch {
			case !ok:
				// not readable now: the next pass's search or re-read sees it
			case m.Mergeable != "MERGEABLE" && m.Mergeable != "CONFLICTING":
				recheck = append(recheck, n) // GitHub recomputing: ask again next pass
			case m.Mergeable != st.PRs[n].Mergeable:
				changed = append(changed, n)
			}
		}
	}
	st.Recheck = recheck
	return changed, nil
}
