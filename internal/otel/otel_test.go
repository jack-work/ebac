package otel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sink struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
	fail   string // a path answered 500
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	s.bodies[r.URL.Path] = append(s.bodies[r.URL.Path], m)
	s.mu.Unlock()
	if r.URL.Path == s.fail || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "refused", 500)
	}
}

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// One round sends one log record (body = the plugin's record, attributes critic/source), one span (ebac.round,
// start..end) and the metrics: a counter adds across rounds from the process start, a gauge replaces; another
// critic's series never ride this critic's export. RED if counters reset each round, gauges add, or critics mix.
func TestExportSendsLogSpanAndCumulativeMetrics(t *testing.T) {
	s := &sink{bodies: map[string][]map[string]any{}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	e := New(srv.URL + "/")
	t0 := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)
	e.start, e.nowFunc = t0.Add(-time.Hour), func() time.Time { return t0 }
	rec := func(deep, left float64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"deep": "x", "metrics": []Metric{
			{Name: "ebac_ghcache_deep_reads_total", Kind: "counter", Value: deep, Attrs: map[string]string{"why": "mark"}},
			{Name: "ebac_ghcache_graphql_points_left", Kind: "gauge", Value: left},
			{Name: "bad_kind", Kind: "histogram", Value: 1},
		}})
		return b
	}
	ctx := context.Background()
	if err := e.Export(ctx, Round{Critic: "gh-cache", Source: "github-cache", Start: t0.Add(-2 * time.Second), End: t0, Record: rec(3, 4800)}); err != nil {
		t.Fatal(err)
	}
	if err := e.Export(ctx, Round{Critic: "other", Source: "github-pr", Start: t0, End: t0, Record: rec(100, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := e.Export(ctx, Round{Critic: "gh-cache", Source: "github-cache", Start: t0, End: t0.Add(time.Second), Record: rec(2, 4790), Error: "marks read: boom"}); err != nil {
		t.Fatal(err)
	}
	logs := s.bodies["/v1/logs"]
	if len(logs) != 3 {
		t.Fatalf("%d log posts", len(logs))
	}
	lr := dig(logs[2], "resourceLogs", 0, "scopeLogs", 0, "logRecords", 0)
	if dig(lr, "severityText") != "ERROR" || !strings.Contains(dig(lr, "body", "stringValue").(string), `"deep":"x"`) {
		t.Fatalf("log record %v", lr)
	}
	if got := dig(logs[0], "resourceLogs", 0, "resource", "attributes", 0, "value", "stringValue"); got != "ebacd" {
		t.Fatalf("service.name %v", got)
	}
	sp := dig(s.bodies["/v1/traces"][0], "resourceSpans", 0, "scopeSpans", 0, "spans", 0)
	if dig(sp, "name") != "ebac.round" || dig(sp, "startTimeUnixNano") != "1795496398000000000" || len(dig(sp, "traceId").(string)) != 32 {
		t.Fatalf("span %v", sp)
	}
	ms := s.bodies["/v1/metrics"]
	if len(ms) != 3 {
		t.Fatalf("%d metric posts", len(ms))
	}
	last := dig(ms[2], "resourceMetrics", 0, "scopeMetrics", 0, "metrics").([]any)
	if len(last) != 2 {
		t.Fatalf("metrics %v (want the counter and the gauge, not the bad kind, not the other critic's)", last)
	}
	if n := len(dig(last[0], "sum", "dataPoints").([]any)); n != 1 {
		t.Fatalf("%d counter points: another critic's series rode this export", n)
	}
	cnt := dig(last[0], "sum", "dataPoints", 0)
	if dig(last[0], "name") != "ebac_ghcache_deep_reads_total" || dig(cnt, "asDouble") != 5.0 || dig(cnt, "startTimeUnixNano") != "1795492800000000000" ||
		dig(last[0], "sum", "aggregationTemporality") != 2.0 || dig(last[0], "sum", "isMonotonic") != true {
		t.Fatalf("counter %v", last[0])
	}
	if g := dig(last[1], "gauge", "dataPoints", 0); dig(last[1], "name") != "ebac_ghcache_graphql_points_left" || dig(g, "asDouble") != 4790.0 {
		t.Fatalf("gauge %v", last[1])
	}
	if a := dig(cnt, "attributes").([]any); len(a) != 3 || dig(a[0], "value", "stringValue") != "gh-cache" || dig(a[2], "key") != "why" {
		t.Fatalf("counter attributes %v", a)
	}
}

// No endpoint exports nothing; a refused path is returned as an error and the other paths are still sent.
func TestExportOffAndErrors(t *testing.T) {
	if err := New("").Export(context.Background(), Round{Critic: "c", Record: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	s := &sink{bodies: map[string][]map[string]any{}, fail: "/v1/logs"}
	srv := httptest.NewServer(s)
	defer srv.Close()
	err := New(srv.URL).Export(context.Background(), Round{Critic: "c", Record: json.RawMessage(`{"metrics":[{"name":"m","kind":"gauge","value":1}]}`)})
	if err == nil || err.Error() != "otlp /v1/logs: HTTP 500: refused" {
		t.Fatalf("err %v", err)
	}
	if len(s.bodies["/v1/traces"]) != 1 || len(s.bodies["/v1/metrics"]) != 1 {
		t.Fatalf("the other paths were not sent: %v", s.bodies)
	}
	n := &sink{bodies: map[string][]map[string]any{}}
	srv2 := httptest.NewServer(n)
	defer srv2.Close()
	if err := New(srv2.URL).Export(context.Background(), Round{Critic: "c", Record: json.RawMessage(`{}`)}); err != nil || len(n.bodies["/v1/metrics"]) != 0 {
		t.Fatalf("a round with no metrics posted metrics: %v %v", err, n.bodies)
	}
}
