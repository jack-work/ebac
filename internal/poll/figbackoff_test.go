package poll

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
)

func TestFigBackoffDelay(t *testing.T) {
	none := func(int64) int64 { return 0 }
	most := func(n int64) int64 { return n - 1 }
	want := []time.Duration{5, 10, 20, 40, 80, 120, 120, 120}
	for i, w := range want {
		w *= time.Second
		if got := figBackoffDelay(i+1, none); got != w {
			t.Errorf("attempt %d: %s, want %s", i+1, got, w)
		}
		if got := figBackoffDelay(i+1, most); got < w*4/5 || got > w {
			t.Errorf("attempt %d jittered: %s, want in [%s, %s]", i+1, got, w*4/5, w)
		}
	}
	if got := figBackoffDelay(64, none); got != figBackoffCap {
		t.Errorf("attempt 64: %s, want the cap", got)
	}
}

func TestPollBacksOffFromAnUnreachableDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EBAC_STATE", dir)
	st, err := form.NewStore(figaro.NewFigaro(filepath.Join(dir, "no-daemon.sock")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := &Poller{Store: st, Fig: st.Fig, Out: &out, BackoffDir: dir, PauseFile: filepath.Join(dir, "pause")}

	err = p.Poll(context.Background(), "rw")
	if err == nil || !strings.Contains(err.Error(), "figaro unreachable (attempt 1)") {
		t.Fatalf("first round: %v", err)
	}
	b := readFigBackoff(dir, "rw")
	if b.Attempt != 1 || !b.Until.After(time.Now()) {
		t.Fatalf("backoff not recorded: %+v", b)
	}
	// Within the backoff: skipped, no call, attempt unchanged.
	if err := p.Poll(context.Background(), "rw"); err != nil || !strings.Contains(out.String(), "backing off from figaro") {
		t.Fatalf("second round: err %v, out %q", err, out.String())
	}
	if readFigBackoff(dir, "rw").Attempt != 1 {
		t.Fatal("a skipped round counted as an attempt")
	}
	// Past it: tried again, attempt 2.
	b.Until = time.Now().Add(-time.Second)
	raw, _ := json.Marshal(b)
	_ = os.WriteFile(figBackoffPath(dir, "rw"), raw, 0o644)
	if err := p.Poll(context.Background(), "rw"); err == nil || !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("third round: %v", err)
	}
	// Another critic keeps its own state.
	if readFigBackoff(dir, "other").Attempt != 0 {
		t.Fatal("backoff leaked across critics")
	}
}

func TestPollPaused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EBAC_STATE", dir)
	st, err := form.NewStore(figaro.NewFigaro(filepath.Join(dir, "no-daemon.sock")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	pause := filepath.Join(dir, "pause")
	p := &Poller{Store: st, Fig: st.Fig, Out: &out, BackoffDir: dir, PauseFile: pause}
	if err := os.WriteFile(pause, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Poll(context.Background(), "rw"); err != nil || strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "round skipped") {
		t.Fatalf("paused: err %v, out %q", err, out.String())
	}
	if readFigBackoff(dir, "rw").Attempt != 0 {
		t.Fatal("a paused round touched the daemon")
	}
	// Control: the pause lifted, the same round reaches for the daemon.
	_ = os.Remove(pause)
	if err := p.Poll(context.Background(), "rw"); err == nil {
		t.Fatal("control: an unpaused round against no daemon succeeded")
	}
}
