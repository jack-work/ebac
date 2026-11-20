package approve

import (
	"context"
	"encoding/json"
	"github.com/gluck/ebac/sdk/grants"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The filer speaks band-grants' request protocol: POST /request with
// credential, ttl_s, reason; GET /request/<id>; an error body surfaces.
func TestFilerFilesAndReads(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "request.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/request":
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["credential"] == "nope" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"credential 'nope' is not grantable to org-approve"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"0123456789ab","state":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/request/0123456789ab":
			_, _ = w.Write([]byte(`{"id":"0123456789ab","state":"approved","credential":"ebac-approve.org","expires_at":5}`))
		default:
			w.WriteHeader(404)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	f := grants.Filer{Socket: sock}
	ctx := context.Background()
	id, err := f.File(ctx, "ebac-approve.org", 2*time.Hour, "APPROVE bic/aether#1 at aaaaaaa")
	if err != nil || id != "0123456789ab" {
		t.Fatalf("file: %q %v", id, err)
	}
	if got["credential"] != "ebac-approve.org" || got["ttl_s"].(float64) != 7200 || got["reason"] != "APPROVE bic/aether#1 at aaaaaaa" {
		t.Fatalf("body %v", got)
	}
	st, err := f.State(ctx, id)
	if err != nil || st.State != "approved" {
		t.Fatalf("state %+v %v", st, err)
	}
	if _, err := f.File(ctx, "nope", time.Hour, "r"); err == nil || !strings.Contains(err.Error(), "not grantable") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := f.State(ctx, "../x"); err == nil {
		t.Fatal("a bad id was read")
	}
}
