package systemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// RetireCriticTimers stops and disables every per-critic timer the manager lists, removes its drop-ins, skips the
// template and names that are not critic names, and does nothing when there are none. RED if retirement skips a
// timer, touches the template, or acts on a path-like name.
func TestRetireCriticTimers(t *testing.T) {
	for _, c := range []struct {
		name    string
		listing string
		retired string
		calls   string
	}{
		{"two critics and the template", "ebac@.timer static -\nebac@author-jack.timer enabled enabled\nebac@review-1.timer linked enabled\n",
			"author-jack,review-1",
			"list-unit-files ebac@*.timer --no-legend --no-pager|stop ebac@author-jack.timer|is-enabled ebac@author-jack.timer|disable --now ebac@author-jack.timer|stop ebac@review-1.timer|is-enabled ebac@review-1.timer|disable --now ebac@review-1.timer"},
		{"none left", "", "", "list-unit-files ebac@*.timer --no-legend --no-pager"},
		{"a path-like name is not ours", "ebac@../x.timer enabled enabled\n", "", "list-unit-files ebac@*.timer --no-legend --no-pager"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			bin := t.TempDir()
			log := filepath.Join(bin, "calls")
			script := "#!/bin/sh\nshift\necho \"$*\" >> " + log + "\ncase \"$1\" in\nlist-unit-files) printf '" + c.listing + "';;\nis-enabled) echo enabled;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			dd := filepath.Join(home, ".config", "systemd", "user", "ebac@author-jack.timer.d")
			if err := os.MkdirAll(dd, 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := RetireCriticTimers()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if strings.Join(got, ",") != c.retired {
				t.Fatalf("%s: retired %q, want %q", c.name, got, c.retired)
			}
			b, _ := os.ReadFile(log)
			var calls []string
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if l != "daemon-reload" { // follows each drop-in removal
					calls = append(calls, l)
				}
			}
			if got := strings.Join(calls, "|"); got != c.calls {
				t.Fatalf("%s: calls\n%s\nwant\n%s", c.name, got, c.calls)
			}
			_, statErr := os.Stat(dd)
			if (c.retired != "") != os.IsNotExist(statErr) {
				t.Fatalf("%s: drop-ins left=%v", c.name, statErr == nil)
			}
		})
	}
}
