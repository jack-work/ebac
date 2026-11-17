package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/forge/forgetest"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

// prDoc is one PR as the single-PR query answered it: one thread whose comment says body, and a second reviews
// page when moreReviews.
func prDoc(n int, body string, moreReviews bool) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{
  "number":%d,"url":"https://x/pull/%d","title":"t%d","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"2026-11-17T00:00:00Z","baseRefName":"main","author":{"login":"a","__typename":"User"},"headRefOid":"sha%d",
  "pending":{"nodes":[]},
  "reviews":{"pageInfo":{"hasNextPage":%v,"endCursor":"c%d"},"nodes":[{"id":"R%d","state":"COMMENTED","submittedAt":"t","body":"r","author":{"login":"x","__typename":"User"}}]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
    {"id":"T%d","isResolved":false,"isOutdated":false,"path":"a.go","line":3,"comments":{"pageInfo":{"hasNextPage":false},"nodes":[
      {"id":"C%d","url":"u","body":%q,"state":"SUBMITTED","createdAt":"1","updatedAt":"1","author":{"login":"amy","__typename":"User"}}]}}]}
}}}}`, n, n, n, n, moreReviews, n, n, n, n, body)
}

func page2Doc(n int) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{
  "number":%d,"url":"https://x/pull/%d","title":"t%d","state":"OPEN","isDraft":false,"merged":false,
  "reviewDecision":"","updatedAt":"2026-11-17T00:00:00Z","baseRefName":"main","author":{"login":"a","__typename":"User"},"headRefOid":"sha%d",
  "pending":{"nodes":[]},
  "reviews":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"R%d-2","state":"APPROVED","submittedAt":"t","body":"r2","author":{"login":"x","__typename":"User"}}]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
  "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}
}}}}`, n, n, n, n, n)
}

type prDir struct {
	t   *testing.T
	dir string
	log string
}

func newPRDir(t *testing.T) *prDir {
	d := &prDir{t: t, dir: t.TempDir()}
	d.log = filepath.Join(d.dir, "calls")
	t.Setenv("EBAC_FAKE_PRS", d.dir)
	t.Setenv("EBAC_FAKE_LOG", d.log)
	return d
}

func (d *prDir) put(name, doc string) {
	if err := os.WriteFile(filepath.Join(d.dir, name), []byte(doc), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

// calls returns and forgets the logged calls.
func (d *prDir) calls() []string {
	b, _ := os.ReadFile(d.log)
	os.Remove(d.log)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func refs(ns ...int) []model.PRRef {
	var out []model.PRRef
	for _, n := range ns {
		out = append(out, model.PRRef{Owner: "acme", Repo: "widget", Number: n})
	}
	return out
}

// Twelve PRs are two calls (forge.PRBatch to a query), not twelve; the one PR with a second reviews page costs one
// more call that asks only it; a PR that is gone fails alone.
func TestFetchPRsAliasesManyPRsIntoOneCall(t *testing.T) {
	d := newPRDir(t)
	var ns []int
	for n := 1; n <= 12; n++ {
		d.put(fmt.Sprintf("%d.json", n), prDoc(n, "hi", n == 3))
		ns = append(ns, n)
	}
	d.put("3.p2.json", page2Doc(3))
	ns = append(ns, 99) // no file: not found
	gh := forgetest.FakeGH(t, "prs-dir")
	sts, errs := gh.FetchPRs(context.Background(), refs(ns...))
	calls := d.calls()
	want := []string{"CALL 1,2,3,4,5,6,7,8,9,10 full", "CALL 11,12,99 full", "CALL 3 full"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q; want %q", calls, want)
	}
	for i := 0; i < 12; i++ {
		if errs[i] != nil || sts[i] == nil || sts[i].Threads[fmt.Sprintf("T%d", i+1)] == nil || !sts[i].Complete {
			t.Fatalf("PR %d: err %v state %+v", i+1, errs[i], sts[i])
		}
	}
	if sts[2].Reviews["R3-2"] == nil || sts[2].Reviews["R3"] == nil {
		t.Fatalf("PR 3's second reviews page not folded in: %v", sts[2].Reviews)
	}
	if errs[12] == nil || !prGone.MatchString(errs[12].Error()) || sts[12] != nil {
		t.Fatalf("the gone PR: err %v state %v; want a not-found error the gone streak counts", errs[12], sts[12])
	}
	if sp := gh.Spent(); sp.Calls != 3 || sp.Points != 3 || sp.Remaining != 4999 {
		t.Fatalf("spend %+v; want 3 calls, 3 points (the fake charges 1 a call), 4999 left", sp)
	}
}

// A review critic probes all its PRs in one call and reads in full only the PRs whose fingerprint moved; every PR
// again once FullReadEvery has passed. A critic of fewer than probeFrom PRs never probes (a full read of one PR
// costs what a probe does).
func TestReadProbesThenReadsOnlyWhatMoved(t *testing.T) {
	d := newPRDir(t)
	for n := 1; n <= 4; n++ {
		d.put(fmt.Sprintf("%d.json", n), prDoc(n, "hi", false))
	}
	gh := forgetest.FakeGH(t, "prs-dir")
	w := &model.Critic{Name: "c", PRs: refs(1, 2, 3, 4)}
	snap := func(sts []*model.PRState) *model.Snapshot {
		s := &model.Snapshot{PRs: map[model.PRKey]*model.PRState{}}
		for _, st := range sts {
			s.PRs[st.Key] = st
		}
		return s
	}
	round := func(prev *model.Snapshot, want ...string) []*model.PRState {
		t.Helper()
		sts, errs := read(context.Background(), gh, w, prev, &plugin.Reply{})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("PR %d: %v", i+1, err)
			}
		}
		if got := d.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("calls %q; want %q", got, want)
		}
		return sts
	}

	first := round(nil, "CALL 1,2,3,4 probe", "CALL 1,2,3,4 full")
	quiet := round(snap(first), "CALL 1,2,3,4 probe")
	for i := range quiet {
		if quiet[i] != first[i] {
			t.Fatalf("PR %d re-read on a quiet round", i+1)
		}
	}
	d.put("2.json", strings.Replace(prDoc(2, "hi", false), `"isResolved":false`, `"isResolved":true`, 1))
	moved := round(snap(quiet), "CALL 1,2,3,4 probe", "CALL 2 full")
	if !moved[1].Threads["T2"].IsResolved || moved[0] != first[0] {
		t.Fatal("the resolved thread not read, or an unmoved PR re-read")
	}
	stale := time.Now().UTC().Add(-FullReadEvery - time.Minute)
	w.Runtime.FullReadAt = &stale
	round(snap(moved), "CALL 1,2,3,4 probe", "CALL 1,2,3,4 full")
	// A PR the previous snapshot lacks is read in full.
	round(snap(moved[:3]), "CALL 1,2,3,4 probe", "CALL 4 full")

	// Control: two PRs, no probe.
	w2 := &model.Critic{Name: "c2", PRs: refs(1, 2)}
	if _, errs := read(context.Background(), gh, w2, nil, &plugin.Reply{}); errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if got := d.calls(); strings.Join(got, "|") != "CALL 1,2 full" {
		t.Fatalf("a 2-PR critic: calls %q; want one full read and no probe", got)
	}
}
