package livetest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fileRemover struct{ log string }

func (r fileRemover) FormRemove(_ context.Context, id string) error {
	f, err := os.OpenFile(r.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("removed " + id + "\n")
	return err
}

// TestHelperFailingLiveTest is not a real test: the child TestFailedLiveTestLeavesNoForm runs. It makes a "critic
// form", registers its end, and fails.
func TestHelperFailingLiveTest(t *testing.T) {
	log := os.Getenv("LIVETEST_FAKE_LOG")
	if log == "" {
		t.Skip("child of TestFailedLiveTestLeavesNoForm only")
	}
	Remove(t, fileRemover{log}, "@crit", "@board")
	t.Fatal("the live test failed")
}

// A live test that FAILS still removes every form it registered.
func TestFailedLiveTestLeavesNoForm(t *testing.T) {
	log := filepath.Join(t.TempDir(), "removed")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFailingLiveTest$", "-test.count=1")
	cmd.Env = append(os.Environ(), "LIVETEST_FAKE_LOG="+log)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(out), "the live test failed") {
		t.Fatalf("the child did not fail as staged: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "removed @crit\nremoved @board\n" {
		t.Fatalf("a failed live test left forms behind: removed %q", b)
	}
}

// A form already gone is fine; any other failed removal is reported, never swallowed.
func TestGone(t *testing.T) {
	for msg, want := range map[string]bool{
		"jsonrpc error -32000: form @x not found":    true,
		"xwal: unknown trunk \"@x\"":                 true,
		"dial unix angelus.sock: connection refused": false,
	} {
		if gone(errors.New(msg)) != want {
			t.Errorf("%q: gone %v", msg, !want)
		}
	}
}
