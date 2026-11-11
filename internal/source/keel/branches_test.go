package keel

import (
	"context"

	"os"
	"strings"
	"testing"
	"time"
)

// cannedDoor serves files captured from keel's own viewer (go test in jack-work/keel viewer at f0036db, a repo with
// main and work/john/effort-x, one diff thread posted through POST /threads), by path.
type cannedDoor map[string]string

func (d cannedDoor) Where() string { return "canned" }
func (d cannedDoor) Get(_ context.Context, path string, _ time.Duration) (int, []byte, error) {
	for prefix, file := range d {
		if strings.HasPrefix(path, prefix) {
			b, err := os.ReadFile("testdata/branches/" + file)
			return 200, b, err
		}
	}
	return 404, []byte("no route"), nil
}

// The branch reads decode keel's real answers.
func TestBranchReadsDecodeKeel(t *testing.T) {
	c := &Client{T: cannedDoor{"/api/r/demo/refs": "refs.json", "/api/r/demo/span": "span.json", "/api/r/demo/log": "log.json",
		"/api/r/demo/threads": "threads_span.json"}}
	ctx := context.Background()
	refs, err := c.Refs(ctx, "demo")
	if err != nil || len(refs) != 2 || refs[1].Name != "work/john/effort-x" || refs[1].Kind != "branch" || len(refs[1].SHA) != 40 || refs[1].Time == 0 {
		t.Fatalf("refs %+v %v", refs, err)
	}
	sp, err := c.Span(ctx, "demo", "main", refs[1].SHA)
	if err != nil || *sp.Count != 1 || sp.Head.SHA != refs[1].SHA || len(sp.Base.SHA) != 40 || len(sp.Files) != 2 ||
		sp.Files[1].Path != "src/new.go" || sp.Files[1].Status != "A" || *sp.Files[0].Additions != 1 {
		t.Fatalf("span %+v %v", sp, err)
	}
	log, err := c.Log(ctx, "demo", refs[1].SHA, 1)
	if err != nil || len(log) != 2 || log[0].SHA != refs[1].SHA || log[0].Subject != "work: two" {
		t.Fatalf("log %+v %v", log, err)
	}
	ths, err := c.Threads(ctx, "demo")
	th := ths["src/main.go"]
	if err != nil || len(th) != 1 || th[0].Origin.Rev != refs[1].SHA || th[0].Origin.Span != "main...work/john/effort-x" ||
		th[0].Anchor.StartLine != 5 || th[0].Comments[0].Alias != "reviewer-a" || th[0].Resolved {
		t.Fatalf("threads %+v %v", ths, err)
	}
	// A body missing what the reader needs is an error, never "nothing".
	for path, body := range map[string]string{"refs": `{"branch":"main"}`, "span": `{"files":[]}`, "log": `null`} {
		c := &Client{T: fixed(body)}
		var err error
		switch path {
		case "refs":
			_, err = c.Refs(ctx, "demo")
		case "span":
			_, err = c.Span(ctx, "demo", "main", "x")
		case "log":
			_, err = c.Log(ctx, "demo", "x", 1)
		}
		if err == nil {
			t.Errorf("%s: %s read as empty", path, body)
		}
	}
}

type fixed string

func (f fixed) Where() string { return "fixed" }
func (f fixed) Get(context.Context, string, time.Duration) (int, []byte, error) {
	return 200, []byte(f), nil
}
