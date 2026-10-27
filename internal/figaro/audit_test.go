package figaro

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/jack-work/figaro/api/rpc"
)

const planted = "PLANTED-SECRET-PAYLOAD-7f3a"

func withAudit(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	auditMu.Lock()
	orig := auditOut
	auditOut = &buf
	auditMu.Unlock()
	t.Cleanup(func() { auditMu.Lock(); auditOut = orig; auditMu.Unlock() })
	return &buf
}

func TestAuditTapLogsMethodAndTargetNeverPayload(t *testing.T) {
	buf := withAudit(t)
	client, server := net.Pipe()
	defer server.Close()
	go io.Copy(io.Discard, server)
	c := auditTap(client)
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "figaro.qua",
		"params": map[string]any{rpc.AriaKey: "a31862792eee330e1ad3674da63124a0", "text": planted}})
	if _, err := c.Write(append(msg, '\n')); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if got != "figaro-rpc figaro.qua a31862792eee330e1ad3674da63124a0\n" {
		t.Fatalf("audit line = %q", got)
	}
	if strings.Contains(got, planted) {
		t.Fatal("payload reached the audit log")
	}
	// Control: the planted value WAS on the wire, so its absence above is the filter's doing.
	if !bytes.Contains(msg, []byte(planted)) {
		t.Fatal("control: planted value not in the request")
	}
}

func TestAuditOffWritesNothing(t *testing.T) {
	auditMu.Lock()
	orig := auditOut
	auditOut = nil
	auditMu.Unlock()
	defer func() { auditMu.Lock(); auditOut = orig; auditMu.Unlock() }()
	client, server := net.Pipe()
	defer server.Close()
	if c := auditTap(client); c != client {
		t.Fatal("tap wrapped the conn with audit off")
	}
	auditRPC("figaro.kill", "x") // must not panic with a nil writer
}

func TestLiveAuditSeesEveryCallOfAFormLifecycle(t *testing.T) {
	f := liveOrSkip(t)
	buf := withAudit(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-audit")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetJSON(ctx, id, "k", planted); err != nil {
		t.Fatal(err)
	}
	cold := NewFigaro("") // a fresh client must attach, so the attach is audited too
	defer cold.Close()
	if _, err := cold.Form(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := f.FormRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"figaro-rpc form.create -\n", "figaro-rpc figaro.attach " + id + "\n", "figaro-rpc figaro.kill " + id + "\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("audit lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, " "+id+"\n") || strings.Contains(got, planted) {
		t.Fatalf("aria-side calls missing or payload leaked:\n%s", got)
	}
	t.Logf("audit:\n%s", got)
}
