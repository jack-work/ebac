package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// One TEMPLATE unit plus a per-instance drop-in, not a daemon that manages
// threads.
//
// Why: a thread-managing daemon means reimplementing restart, backoff,
// per-critic isolation and log separation that systemd already has, and one
// panicking critic takes every other critic down with it. Type=oneshot also
// gives free self-throttling — systemd will not start a second instance of
// an instance that is still running, so a slow round delays the next one
// instead of stampeding GitHub.
//
// (prangl's runbook says "do not create one heartbeat per PR". That rule
// existed because prangl's state identity was the whole aria+PR-set tuple,
// so splitting it fragmented one state file. Here state is per-CRITIC and a
// critic may hold many PRs, so the rule is satisfied by grouping PRs into a
// critic, not by collapsing critics into one timer.)

const serviceTemplate = `[Unit]
Description=ebac PR review monitor (%i)
Documentation=file://%HOME%/dev/ebac/README.md
After=network-online.target

[Service]
Type=oneshot
ExecStart=%BIN% poll --critic %i
TimeoutStartSec=300
Environment=NO_COLOR=1
Environment=GH_PAGER=
# A user timer inherits almost nothing. ebac pins absolute paths to gh and
# figaro in the critic file, but a subprocess of gh (git, its credential
# helper) still needs a usable PATH.
Environment=PATH=%PATH%
# A poll failure is normal (network, expired token). Do not let it mark the
# unit failed forever; the next timer tick is the retry.
SuccessExitStatus=0
StandardOutput=journal
StandardError=journal
`

const timerTemplate = `[Unit]
Description=ebac heartbeat (%i)

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min
AccuracySec=30s
# Stagger instances so twenty critics do not hit GitHub in the same second.
RandomizedDelaySec=45s
Unit=ebac@%i.service

[Install]
WantedBy=timers.target
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
	svc := strings.ReplaceAll(serviceTemplate, "%BIN%", binPath)
	svc = strings.ReplaceAll(svc, "%HOME%", home)
	svc = strings.ReplaceAll(svc, "%PATH%", unitPath(home))
	if err := os.WriteFile(filepath.Join(dir, "ebac@.service"), []byte(svc), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "ebac@.timer"), []byte(timerTemplate), 0o644); err != nil {
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

// setInterval writes a per-instance drop-in. A template unit cannot carry a
// per-instance interval, but a drop-in on the instance can.
func setInterval(name string, d time.Duration) error {
	dir, err := unitDir()
	if err != nil {
		return err
	}
	dd := filepath.Join(dir, fmt.Sprintf("ebac@%s.timer.d", name))
	if err := os.MkdirAll(dd, 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("[Timer]\nOnUnitActiveSec=%ds\nOnBootSec=60s\n", int(d.Seconds()))
	return os.WriteFile(filepath.Join(dd, "interval.conf"), []byte(body), 0o644)
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

func EnableTimer(name string, interval time.Duration) error {
	if err := setInterval(name, interval); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", "--now", fmt.Sprintf("ebac@%s.timer", name))
}

func disableTimer(name string) error {
	unit := fmt.Sprintf("ebac@%s.timer", name)
	if _, err := systemctlOut("is-enabled", unit); err != nil {
		// Not enabled: nothing to do, and that is not a failure.
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

// NextElapse reports when the timer next fires.
//
// `systemctl show -p NextElapseUSecRealtime` returns 0 for a MONOTONIC timer
// (OnUnitActiveSec), which is exactly the kind ebac installs — so the
// obvious property reads "no next elapse" for every healthy timer we own.
// That is a false absence: the timer is scheduled and systemd knows when.
// Ask the question the way that can actually return the answer.
func NextElapse(name string) string {
	unit := fmt.Sprintf("ebac@%s.timer", name)
	if out, err := systemctlOut("show", unit, "-p", "NextElapseUSecRealtime", "--value"); err == nil {
		if out != "" && out != "0" && out != "n/a" {
			return out
		}
	}
	out, err := systemctlOut("list-timers", unit, "--all", "--no-pager", "--no-legend")
	if err != nil || out == "" {
		return "-"
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, unit) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			return strings.Join(fields[:4], " ")
		}
		return line
	}
	return "-"
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
