package poll

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gluck/ebac/internal/otel"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// A plugin call's telemetry record reaches the exporter (one log post carries it); a record over MaxTelemetry is not
// exported and is said; no exporter or no record sends nothing. RED if apply stops exporting or the cap goes.
func TestAPluginCallExportsItsRecord(t *testing.T) {
	var posts atomic.Int32
	var logBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.URL.Path == "/v1/logs" {
			b, _ := io.ReadAll(r.Body)
			logBody.Store(string(b))
		}
	}))
	defer srv.Close()
	var out strings.Builder
	w := &model.Critic{Name: "gh-cache", Source: model.SourceGHCache}
	handle := func(_ context.Context, req plugin.Request) (*plugin.Reply, error) {
		return &plugin.Reply{Telemetry: json.RawMessage(`{"deep":[{"pr":7,"why":"mark"}]}`)}, nil
	}
	p := &Poller{Out: &out, OTel: otel.New(srv.URL), Plugins: plugin.InProc{"gh": handle}}
	if _, err := p.call(context.Background(), w, plugin.Request{Op: plugin.OpRound}); err != nil { // the plugin call exports
		t.Fatal(err)
	}
	if posts.Load() != 2 { // logs + traces; no metrics member
		t.Fatalf("%d posts", posts.Load())
	}
	if s, _ := logBody.Load().(string); !strings.Contains(s, `\"why\":\"mark\"`) {
		t.Fatalf("log body %q", s)
	}
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", MaxTelemetry) + `"}`)
	p.export(context.Background(), w, &plugin.Reply{Telemetry: big})
	if posts.Load() != 2 || !strings.Contains(out.String(), "gh-cache: telemetry record of 65544 bytes over 65536: not exported") {
		t.Fatalf("over the cap: %d posts, out %q", posts.Load(), out.String())
	}
	(&Poller{Out: &out}).export(context.Background(), w, &plugin.Reply{Telemetry: json.RawMessage(`{}`)})
	p.export(context.Background(), w, &plugin.Reply{})
	if posts.Load() != 2 {
		t.Fatalf("sent without an exporter or a record: %d", posts.Load())
	}
}
