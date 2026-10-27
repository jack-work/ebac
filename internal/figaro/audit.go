package figaro

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/jack-work/figaro/api/rpc"
	"github.com/jack-work/figaro/sdk"
)

// RPC audit (access table v2, piece 4c, INTERIM figaro#47): while uid ebac
// holds a broad grant on the figaro daemon, every RPC it makes is logged as
// one line, method and target, never a payload, so misuse would be visible.
// On with EBAC_RPC_AUDIT=1 (the units set it); off for an operator's CLI.
//
// Two sides, because the SDK taps only per-aria connections: auditTap parses
// each request written on an aria's connection; daemon-level calls (attach,
// create, form.create, kill, fork, list) log through auditRPC at their call
// sites in figaro.go, formstate.go and lease.go.

var (
	auditMu  sync.Mutex
	auditOut io.Writer = auditWriter()
)

func auditWriter() io.Writer {
	if os.Getenv("EBAC_RPC_AUDIT") == "1" {
		return os.Stderr
	}
	return nil
}

func auditRPC(method, target string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	if auditOut == nil {
		return
	}
	if target == "" {
		target = "-"
	}
	fmt.Fprintf(auditOut, "figaro-rpc %s %s\n", method, target)
}

// auditTap wraps an aria connection: jkrpc encodes one JSON message per
// Write, so each Write is one request. Only the method and the aria id are
// read; the params are never logged.
func auditTap(c net.Conn) net.Conn {
	if auditOut == nil {
		return c
	}
	return &auditConn{Conn: c}
}

type auditConn struct{ net.Conn }

func (a *auditConn) Write(p []byte) (int, error) {
	var m struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if json.Unmarshal(p, &m) == nil && m.Method != "" {
		var target string
		_ = json.Unmarshal(m.Params[rpc.AriaKey], &target)
		auditRPC(m.Method, target)
	}
	return a.Conn.Write(p)
}

// auditAng logs one daemon-level call and hands back the client, so a call
// site reads auditAng(method, target, ang).Method(...).
func AuditAng(method, target string, ang *sdk.Angelus) *sdk.Angelus {
	auditRPC(method, target)
	return ang
}
