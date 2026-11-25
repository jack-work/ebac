package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gluck/ebac/sdk/forge"
)

// The booking parser's pattern for ebac's journal (copied verbatim 11-25): what a verb's line must match to be booked as ebac:<verb>.
var healthMetricsEbacRE = regexp.MustCompile(`^([^:]+): (?:cost: )?graphql (\d+) call\(s\), (\d+) point\(s\)`)

// #76 (a): a verb's cost line is the critic round's shape, so health-metrics books it as ebac:<verb>. RED if the line
// changes shape or names something else.
func TestAVerbsCostLineIsBookedAsEbacVerb(t *testing.T) {
	line := verbCostLine("comment-edit", forge.Spend{All: 3, Calls: 3, Points: 3, Remaining: 4722})
	if want := "comment-edit: graphql 3 call(s), 3 point(s), 4722 left; rest 0 call(s)"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	m := healthMetricsEbacRE.FindStringSubmatch(line)
	if m == nil || m[1] != "comment-edit" || m[2] != "3" || m[3] != "3" {
		t.Fatalf("health-metrics reads %q from %q", m, line)
	}
}

// A verb that called gh not at all prints no line (nothing to book).
func TestAVerbThatCalledNothingHasNoCostLine(t *testing.T) {
	if line := verbCostLine("ls", forge.Spend{Remaining: -1}); line != "" {
		t.Fatalf("line = %q, want none", line)
	}
}

type journalRec struct {
	lines []string
	err   error
}

func (j *journalRec) send(l string) error {
	j.lines = append(j.lines, l)
	return j.err
}

// The line goes to stderr and, when stderr is not the journal already, to the journal; the daemon prints none (its
// rounds book per critic). RED if the journal send goes (a sudo'd verb's stderr never reaches ebac's journal).
func TestReportVerbCostPrintsAndJournals(t *testing.T) {
	s := forge.Spend{All: 2, Calls: 2, Points: 2, Remaining: 4800}
	const line = "comment-edit: graphql 2 call(s), 2 point(s), 4800 left; rest 0 call(s)"
	for _, c := range []struct {
		name      string
		cmd       string
		toJournal bool
		jerr      error
		out       string
		journaled []string
	}{
		{"a seat's call", "comment-edit", true, nil, line + "\n", []string{line}},
		{"a unit's run (stderr is the journal)", "comment-edit", false, nil, line + "\n", nil},
		{"the journal refuses", "comment-edit", true, errors.New("no socket"), line + "\nebac: cost line not journaled: no socket\n", []string{line}},
		{"the daemon", "daemon", true, nil, "", nil},
	} {
		var out strings.Builder
		j := &journalRec{err: c.jerr}
		reportVerbCost(c.cmd, s, &out, c.toJournal, j.send)
		if out.String() != c.out || strings.Join(j.lines, "|") != strings.Join(c.journaled, "|") {
			t.Errorf("%s: out %q journal %q, want %q and %q", c.name, out.String(), j.lines, c.out, c.journaled)
		}
	}
}

// journalSend writes journald's native frame on the socket: identifier ebac, info, the line as MESSAGE.
func TestJournalSendWritesTheNativeFrame(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "j.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	old := journalSocket
	journalSocket = sock
	t.Cleanup(func() { journalSocket = old })
	if err := journalSend("comment-edit: graphql 1 call(s), 1 point(s), 9 left; rest 0 call(s)"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SYSLOG_IDENTIFIER=ebac\nPRIORITY=6\nMESSAGE=comment-edit: graphql 1 call(s), 1 point(s), 9 left; rest 0 call(s)\n"; string(buf[:n]) != want {
		t.Fatalf("frame = %q, want %q", buf[:n], want)
	}
}

// JOURNAL_STREAM names stderr's device:inode when a unit's stderr is the journal; anything else is not.
func TestStderrIsJournalOnlyWhenTheStreamIsStderr(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	dev, ino := strconv.FormatUint(uint64(st.Dev), 10), strconv.FormatUint(st.Ino, 10)
	for _, c := range []struct {
		stream string
		want   bool
	}{
		{dev + ":" + ino, true},
		{dev + ":" + ino + "1", false},
		{"", false},
		{"garbage", false},
	} {
		if got := stderrIsJournal(c.stream, f); got != c.want {
			t.Errorf("stream %q: %v, want %v", c.stream, got, c.want)
		}
	}
}

// Only a seat's one-shot call is paced: not the daemon, not a unit's run (no caller).
func TestOnlyASeatsOneShotCallIsPaced(t *testing.T) {
	for _, c := range []struct {
		cmd, caller string
		want        bool
	}{
		{"comment-edit", "139b7bb5", true},
		{"post", "139b7bb5", true},
		{"comment-edit", "", false},
		{"reconcile", "", false},
		{"daemon", "139b7bb5", false},
	} {
		if got := pacesCalls(c.cmd, c.caller); got != c.want {
			t.Errorf("%s caller %q: %v, want %v", c.cmd, c.caller, got, c.want)
		}
	}
}

// A seat's poll runs its plugins as child processes: their spend is booked by each round's cost line, which on a
// seat's call must reach the journal too (review r1 #2). The tee journals exactly the cost lines, whole, even when a
// line arrives in pieces, and passes everything through. RED if the tee journals nothing or every line.
func TestASeatsRoundCostLinesReachTheJournal(t *testing.T) {
	var out strings.Builder
	j := &journalRec{}
	tee := &costTee{w: &out, journal: j.send}
	in := "gh-cache: cost: graphql 4 call(s), 9 point(s), 4700 left; rest 2 call(s)\nother: no cost here\nreview-1: graph"
	for _, chunk := range []string{in[:30], in[30:], "ql 1 call(s), 1 point(s), 4699 left; rest 0 call(s)\n"} {
		if _, err := tee.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"gh-cache: cost: graphql 4 call(s), 9 point(s), 4700 left; rest 2 call(s)",
		"review-1: graphql 1 call(s), 1 point(s), 4699 left; rest 0 call(s)"}
	if strings.Join(j.lines, "|") != strings.Join(want, "|") {
		t.Fatalf("journaled %q, want %q", j.lines, want)
	}
	if out.String() != in+"ql 1 call(s), 1 point(s), 4699 left; rest 0 call(s)\n" {
		t.Fatalf("passed through %q", out.String())
	}
}

// Only a seat's call tees: a unit's output is the journal already. RED if a unit's round lines are journaled twice.
func TestRoundOutTeesOnlyASeatsCall(t *testing.T) {
	old := journalSeat
	t.Cleanup(func() { journalSeat = old })
	var w strings.Builder
	journalSeat = false
	if got := roundOut(&w); got != &w {
		t.Fatalf("unit: roundOut = %T, want the writer itself", got)
	}
	journalSeat = true
	if _, ok := roundOut(&w).(*costTee); !ok {
		t.Fatalf("seat: roundOut = %T, want a *costTee", roundOut(&w))
	}
}
