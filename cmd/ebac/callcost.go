package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/gluck/ebac/internal/callpace"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/sdk/forge"
)

// A one-shot verb's GraphQL spend is booked the way a critic round's is: one
// line "<verb>: graphql N call(s), P point(s), L left; rest R call(s)" that
// health-metrics reads from ebac's journal and books as ebac:<verb> (#76: 75
// comment-edit calls spent 156 points no ledger saw).

// verbCostLine is cmd's cost line, or "" when the verb called gh not at all.
func verbCostLine(cmd string, s forge.Spend) string {
	if s.All == 0 {
		return ""
	}
	return cmd + ": " + s.String()
}

// journalSocket is journald's native socket (a test points it elsewhere).
var journalSocket = "/run/systemd/journal/socket"

// journalSend writes line to the journal as this process (journald stamps
// _UID from the socket's peer: a sudo'd verb lands as ebac's).
func journalSend(line string) error {
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: journalSocket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte("SYSLOG_IDENTIFIER=ebac\nPRIORITY=6\nMESSAGE=" + strings.ReplaceAll(line, "\n", " ") + "\n"))
	return err
}

// stderrIsJournal says stderr already goes to the journal (a systemd unit's
// StandardError=journal: JOURNAL_STREAM names stderr's device and inode), so
// the line must not be sent twice.
func stderrIsJournal(stream string, stderr *os.File) bool {
	dev, ino, ok := strings.Cut(stream, ":")
	if !ok {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(stderr.Fd()), &st); err != nil {
		return false
	}
	return strconv.FormatUint(uint64(st.Dev), 10) == dev && strconv.FormatUint(st.Ino, 10) == ino
}

// reportVerbCost prints cmd's cost line on w and, unless w already reaches
// the journal, sends it there too. The daemon books its rounds per critic.
func reportVerbCost(cmd string, s forge.Spend, w io.Writer, toJournal bool, journal func(string) error) {
	if cmd == "daemon" {
		return
	}
	line := verbCostLine(cmd, s)
	if line == "" {
		return
	}
	fmt.Fprintln(w, line)
	if toJournal {
		if err := journal(line); err != nil {
			fmt.Fprintf(w, "ebac: cost line not journaled: %v\n", err)
		}
	}
}

// pacesCalls says the verb's GraphQL calls are paced: a seat's one-shot call
// through the operator wrapper (a caller), never the daemon or a unit's run.
func pacesCalls(cmd, caller string) bool {
	return caller != "" && cmd != "daemon"
}

// installPacer paces this process's GraphQL calls on the shared budget.
func installPacer(note io.Writer) (restore func()) {
	root, err := form.StateRoot()
	if err != nil {
		fmt.Fprintf(note, "ebac: call pace: no state root, not paced: %v\n", err)
		return func() {}
	}
	return forge.SetPacer(callpace.New(root, note))
}

// journalSeat says this process's own output does not reach the journal (a
// seat's call through sudo, not a unit's run): set once in main.
var journalSeat bool

// roundCostLine is a critic round's cost line, the shape health-metrics books
// as ebac:<critic>.
var roundCostLine = regexp.MustCompile(`^[^:]+: (?:cost: )?graphql \d+ call\(s\), \d+ point\(s\)`)

// costTee passes everything to w and journals each complete cost line.
type costTee struct {
	w       io.Writer
	journal func(string) error
	buf     []byte
}

func (t *costTee) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := string(t.buf[:i])
		t.buf = t.buf[i+1:]
		if roundCostLine.MatchString(line) {
			_ = t.journal(line)
		}
	}
	return n, err
}

// roundOut is where a one-shot verb prints its plugin rounds' lines. A
// plugin runs as its own process, so its spend is not this process's: its
// round's cost line books it, and on a seat's call that line must reach the
// journal too (a unit's output already does).
func roundOut(w io.Writer) io.Writer {
	if !journalSeat {
		return w
	}
	return &costTee{w: w, journal: journalSend}
}
