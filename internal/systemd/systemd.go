package systemd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluck/ebac/sdk/model"
)

// ebacd is ONE long-running user service that schedules every critic's
// round (internal/sched). It replaced one ebac@<critic>.timer per critic
// (2026-11-19): a timer per critic could not share a budget, so twenty
// critics spent one GitHub account's quota with no one counting. Per-critic
// isolation is kept where it matters: each round's source plugin is its own
// sandboxed process, killed at the round's deadline.
const ebacdService = `[Unit]
Description=ebacd: schedule every ebac critic's round under the shared budget
After=network-online.target

[Service]
Type=simple
ExecStart=%BIN% daemon
Restart=always
RestartSec=10
Environment=NO_COLOR=1
Environment=GH_PAGER=
Environment=PATH=%PATH%
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
`

// The reconciler is a SINGLE unit, not a template: it is the only writer of
// the roster index, and two of them racing would reintroduce exactly the lost
// update the single-writer design exists to prevent. It runs rarely because
// every rule it applies is a write somebody else might be making.
const reconcileService = `[Unit]
Description=ebac reconciler — rectify drift between critics, forms and timers

[Service]
Type=oneshot
ExecStart=%BIN% reconcile
Environment=NO_COLOR=1
Environment=PATH=%PATH%
StandardOutput=journal
StandardError=journal
`

const reconcileTimer = `[Unit]
Description=ebac reconcile (every 30m)

[Timer]
OnBootSec=5min
OnUnitActiveSec=30min
AccuracySec=2min
RandomizedDelaySec=3min
Unit=ebac-reconcile.service

[Install]
WantedBy=timers.target
`

// The promoter is a single unit on a SLOW heartbeat. It only ever acts when
// a slot is free, and slots free themselves when critics archive, so running
// it often buys nothing and costs a GitHub call per pending item.
const queueService = `[Unit]
Description=ebac queue promoter — take PRs from the queue while under budget

[Service]
Type=oneshot
ExecStart=%BIN% queue promote
Environment=NO_COLOR=1
Environment=PATH=%PATH%
StandardOutput=journal
StandardError=journal
`

const queueTimer = `[Unit]
Description=ebac queue promotion (every 10m)

[Timer]
OnBootSec=3min
OnUnitActiveSec=10min
AccuracySec=1min
RandomizedDelaySec=2min
Unit=ebac-queue.service

[Install]
WantedBy=timers.target
`

func unitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".config", "systemd", "user")
	return d, os.MkdirAll(d, 0o755)
}

func InstallUnits(binPath string) (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	if binPath == "" {
		binPath, _ = exec.LookPath("ebac")
	}
	if binPath == "" {
		binPath = filepath.Join(home, "go", "bin", "ebac")
	}
	abs, err := filepath.Abs(binPath)
	if err == nil {
		binPath = abs
	}
	svc := strings.ReplaceAll(ebacdService, "%BIN%", binPath)
	svc = strings.ReplaceAll(svc, "%PATH%", unitPath(home))
	if err := os.WriteFile(filepath.Join(dir, "ebacd.service"), []byte(svc), 0o644); err != nil {
		return "", err
	}
	rsvc := strings.ReplaceAll(reconcileService, "%BIN%", binPath)
	rsvc = strings.ReplaceAll(rsvc, "%PATH%", unitPath(home))
	if err := os.WriteFile(filepath.Join(dir, "ebac-reconcile.service"), []byte(rsvc), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "ebac-reconcile.timer"), []byte(reconcileTimer), 0o644); err != nil {
		return "", err
	}
	qsvc := strings.ReplaceAll(queueService, "%BIN%", binPath)
	qsvc = strings.ReplaceAll(qsvc, "%PATH%", unitPath(home))
	if err := os.WriteFile(filepath.Join(dir, "ebac-queue.service"), []byte(qsvc), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "ebac-queue.timer"), []byte(queueTimer), 0o644); err != nil {
		return "", err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return "", err
	}
	return dir, nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func systemctlOut(args ...string) (string, error) {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// removeTimerDropIns deletes the critic's ebac@<name>.timer.d (its interval
// drop-ins): a removed critic left them behind, and they piled up in
// ~/.config/systemd/user for every critic that ever ran.
func RemoveTimerDropIns(name string) error {
	if err := model.ValidName(name); err != nil {
		return err
	}
	dir, err := unitDir()
	if err != nil {
		return err
	}
	dd := filepath.Join(dir, fmt.Sprintf("ebac@%s.timer.d", name))
	if _, err := os.Stat(dd); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(dd); err != nil {
		return fmt.Errorf("remove %s: %w", dd, err)
	}
	return systemctl("daemon-reload")
}

func DisableTimer(name string) error {
	unit := fmt.Sprintf("ebac@%s.timer", name)
	_, _ = systemctlOut("stop", unit) // a runtime-started timer is active without being enabled
	if _, err := systemctlOut("is-enabled", unit); err != nil {
		// Not enabled: nothing more to do, and that is not a failure.
		return nil
	}
	return systemctl("disable", "--now", unit)
}

// TimerStatus is what systemd thinks, as PROPERTIES rather than as a string
// to be matched later.
//
// The first version of this returned "active/enabled" and callers compared
// against literals like "inactive/disabled". systemd actually emits
// "not-found" for a unit that was never installed, so the roster's health
// column scored an unarmed critic as "ok" while the selfcheck correctly
// called it unarmed. Match the fact, not the shape it arrived in.
type TimerStatus struct {
	Active  bool
	Enabled bool
	Known   bool
	Display string
}

func (t TimerStatus) Armed() bool { return t.Known && t.Active && t.Enabled }

func TimerState(name string) TimerStatus {
	unit := fmt.Sprintf("ebac@%s.timer", name)
	activeOut, activeErr := systemctlOut("is-active", unit)
	enabledOut, _ := systemctlOut("is-enabled", unit)

	t := TimerStatus{}
	t.Active = activeErr == nil && activeOut == "active"
	// An ELAPSED timer is "active" but will never fire again (every trigger is
	// in the past: a drop-in that wiped OnUnitActiveSec left exactly that, and
	// ls said "armed"). It is not armed: R3 re-arms it, selfcheck flags it.
	if t.Active {
		if sub, _ := systemctlOut("show", unit, "-p", "SubState", "--value"); sub == "elapsed" {
			t.Active, activeOut = false, "elapsed"
		}
	}
	switch enabledOut {
	case "enabled", "enabled-runtime", "static", "linked", "linked-runtime", "indirect", "generated":
		t.Enabled, t.Known = true, true
	case "disabled", "masked", "masked-runtime":
		t.Enabled, t.Known = false, true
	default:
		// "not-found", an error string, or anything unrecognised: the unit
		// is not installed as far as we can tell. Say so; do not fold it
		// into "disabled", which would imply it exists.
		t.Enabled, t.Known = false, false
	}
	switch {
	case !t.Known:
		t.Display = "absent"
	case t.Active && t.Enabled:
		t.Display = "armed"
	case t.Enabled:
		t.Display = "enabled/" + activeOut
	default:
		t.Display = "disabled"
	}
	return t
}

// unitPath builds a PATH for the unit from the operator's current one,
// keeping only entries that exist. Copying $PATH verbatim would bake in
// whatever a shell happened to prepend.
func unitPath(home string) string {
	var keep []string
	seen := map[string]bool{}
	for _, d := range append(strings.Split(os.Getenv("PATH"), ":"), []string{
		filepath.Join(home, ".nix-profile", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		"/run/current-system/sw/bin", "/usr/local/bin", "/usr/bin", "/bin",
	}...) {
		if d == "" || seen[d] {
			continue
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		seen[d] = true
		keep = append(keep, d)
	}
	return strings.Join(keep, ":")
}

// RetireCriticTimers disables every per-critic ebac@<name>.timer still loaded
// in this user manager and removes its interval drop-ins: ebacd schedules
// every critic, and a leftover timer would run a second, unbudgeted round.
// It returns the names it retired. Reverting the ebacd pin brings the timers
// back (ebac reconcile R3 of the older ebac re-arms them).
func RetireCriticTimers() ([]string, error) {
	out, err := systemctlOut("list-unit-files", "ebac@*.timer", "--no-legend", "--no-pager")
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(out) == "" {
		return nil, nil // list-unit-files exits 1 when no unit matches: none left
	}
	if err != nil && out == "" {
		return nil, fmt.Errorf("list ebac@ timers: %v", err)
	}
	var retired, errs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		name, ok := strings.CutPrefix(strings.TrimSuffix(f[0], ".timer"), "ebac@")
		if !ok || name == "" || model.ValidName(name) != nil {
			continue // the template itself (ebac@.timer), or not ours
		}
		// Each timer on its own: one that fails does not keep the rest armed
		// (11-19: disable printed "Removed ..." and exited non-zero on the
		// first, and the rest stayed enabled).
		if err := DisableTimer(name); err != nil {
			errs = append(errs, err.Error())
		}
		if err := RemoveTimerDropIns(name); err != nil {
			errs = append(errs, err.Error())
		}
		retired = append(retired, name)
	}
	if len(errs) > 0 {
		return retired, fmt.Errorf("%d error(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return retired, nil
}
