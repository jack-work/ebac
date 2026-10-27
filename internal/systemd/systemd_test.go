package systemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func TestRemoveTimerDropIns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	dd := filepath.Join(home, ".config", "systemd", "user", "ebac@gone-critic.timer.d")
	keep := filepath.Join(home, ".config", "systemd", "user", "ebac@other.timer.d")
	for _, d := range []string{dd, keep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(d, "interval.conf"), []byte("[Timer]\n"), 0o644)
	}
	if err := RemoveTimerDropIns("gone-critic"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dd); !os.IsNotExist(err) {
		t.Fatalf("drop-in dir survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("another critic's drop-ins were touched: %v", err)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "daemon-reload") {
		t.Fatalf("no daemon-reload: %q", b)
	}
	if err := RemoveTimerDropIns("never-existed"); err != nil {
		t.Fatalf("absent dir is not an error: %v", err)
	}
	if err := RemoveTimerDropIns("../escape"); err == nil {
		t.Fatal("a path-like name was accepted")
	}
}

// A drop-in that does not reset OnUnitActiveSec ADDS to the template's 5 minutes.
// Reconcile (R3) re-armed a 60 s Teams critic at a hard-coded 5m after a reboot.
func TestEnableTimerResetsTheTemplateAndUsesTheCriticsInterval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	w := &model.Critic{Name: "every-test", EverySec: 60}
	if err := EnableTimer(w.Name, w.Interval()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "ebac@every-test.timer.d", "interval.conf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// ANY empty On*Sec= assignment resets the whole timer list: exactly one, first.
	if strings.Count(got, "Sec=\n") != 1 || !strings.HasPrefix(got, "[Timer]\nOnUnitActiveSec=\n") {
		t.Fatalf("drop-in must reset the timer list exactly once, first:\n%s", got)
	}
	for _, want := range []string{"OnBootSec=60s\n", "OnUnitActiveSec=60s\n", "RandomizedDelaySec=6s\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("drop-in lacks %q:\n%s", want, got)
		}
	}
	if (&model.Critic{}).Interval() != 5*time.Minute || (&model.Critic{EverySec: 600}).Interval() != 10*time.Minute {
		t.Fatal("Interval default/explicit wrong")
	}
	// Jitter is capped at the template's 45 s.
	if err := EnableTimer("every-test", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "ebac@every-test.timer.d", "interval.conf"))
	if !strings.Contains(string(b), "RandomizedDelaySec=45s\n") {
		t.Fatalf("jitter not capped:\n%s", b)
	}
}

// An elapsed timer (all triggers in the past) is "active" to is-active but will
// never fire again; it must not read as armed.
func TestTimerStateElapsedIsNotArmed(t *testing.T) {
	for _, c := range []struct {
		sub   string
		armed bool
		show  string
	}{{"elapsed", false, "enabled/elapsed"}, {"waiting", true, "armed"}} {
		bin := t.TempDir()
		script := "#!/bin/sh\ncase \"$*\" in\n*is-active*) echo active;;\n*is-enabled*) echo enabled;;\n*SubState*) echo " + c.sub + ";;\nesac\n"
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		ts := TimerState("x")
		if ts.Armed() != c.armed || ts.Display != c.show {
			t.Fatalf("substate %s: armed=%v display=%q; want %v %q", c.sub, ts.Armed(), ts.Display, c.armed, c.show)
		}
	}
}
