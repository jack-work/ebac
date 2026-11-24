// Package otel exports ebac's round records to the node's OpenTelemetry agent
// over OTLP/HTTP with JSON encoding (no SDK): one log record per round, one
// span per round, and the round's metrics as cumulative counters and gauges
// kept in this process. The agent (127.0.0.1:4318) forwards to the fleet's
// metrics, logs and traces stores. Export never fails a round: an error is
// returned for the caller to log, and the next round sends again.
package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServiceName is the resource's service.name.
const ServiceName = "ebacd"

// Metric is one value a round reports: a counter adds Value to its running
// total, a gauge replaces its value.
type Metric struct {
	Name  string            `json:"name"`
	Kind  string            `json:"kind"` // counter | gauge
	Value float64           `json:"value"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Round is what one export sends.
type Round struct {
	Critic, Source string
	Start, End     time.Time
	Record         json.RawMessage // the plugin's record (log body); its "metrics" member is read
	Error          string          // the round failed
}

// Exporter sends to Endpoint (e.g. http://127.0.0.1:4318). The zero
// Endpoint exports nothing.
type Exporter struct {
	Endpoint string
	Client   *http.Client
	Host     string // host.name (the agent forces its own; set for tests)

	mu      sync.Mutex
	start   time.Time
	series  map[string]*point // key: name + sorted attrs
	nowFunc func() time.Time
}

type point struct {
	critic     string
	name, kind string
	attrs      map[string]string
	value      float64
}

// New returns an exporter for endpoint; "" disables it.
func New(endpoint string) *Exporter {
	return &Exporter{Endpoint: strings.TrimRight(endpoint, "/"), Client: &http.Client{Timeout: 3 * time.Second,
		Transport: &http.Transport{Proxy: nil}}, start: time.Now(), series: map[string]*point{}}
}

func (e *Exporter) now() time.Time {
	if e.nowFunc != nil {
		return e.nowFunc()
	}
	return time.Now()
}

func key(critic string, m Metric) string {
	ks := make([]string, 0, len(m.Attrs))
	for k, v := range m.Attrs {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return critic + "/" + m.Name + "{" + strings.Join(ks, ",") + "}"
}

// Export sends the round's log record, span and metrics. It returns the
// first error (the others are still tried).
func (e *Exporter) Export(ctx context.Context, r Round) error {
	if e == nil || e.Endpoint == "" {
		return nil
	}
	var rec struct {
		Metrics []Metric `json:"metrics"`
	}
	_ = json.Unmarshal(r.Record, &rec)
	e.mu.Lock()
	for _, m := range rec.Metrics {
		if m.Name == "" || (m.Kind != "counter" && m.Kind != "gauge") {
			continue
		}
		k := key(r.Critic, m)
		p := e.series[k]
		if p == nil {
			p = &point{critic: r.Critic, name: m.Name, kind: m.Kind, attrs: m.Attrs}
			e.series[k] = p
		}
		if m.Kind == "counter" {
			p.value += m.Value
		} else {
			p.value = m.Value
		}
	}
	metrics := e.metricsBody(r.Critic, r.Source)
	e.mu.Unlock()
	var first error
	for _, b := range []struct {
		path string
		body any
	}{
		{"/v1/logs", e.logsBody(r)},
		{"/v1/traces", e.tracesBody(r)},
		{"/v1/metrics", metrics},
	} {
		if m, ok := b.body.(kv); ok && m == nil {
			continue // no metrics for this critic
		}
		if err := e.post(ctx, b.path, b.body); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (e *Exporter) post(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("otlp %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("otlp %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

type kv = map[string]any

func str(k, v string) kv { return kv{"key": k, "value": kv{"stringValue": v}} }

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (e *Exporter) resource() kv {
	attrs := []kv{str("service.name", ServiceName)}
	if e.Host != "" {
		attrs = append(attrs, str("host.name", e.Host))
	}
	return kv{"attributes": attrs}
}

func (e *Exporter) logsBody(r Round) kv {
	sev, num := "INFO", 9
	if r.Error != "" {
		sev, num = "ERROR", 17
	}
	attrs := []kv{str("critic", r.Critic), str("source", r.Source), str("event.name", "ebac.round")}
	if r.Error != "" {
		attrs = append(attrs, str("error", r.Error))
	}
	return kv{"resourceLogs": []kv{{"resource": e.resource(), "scopeLogs": []kv{{"scope": kv{"name": "ebac"},
		"logRecords": []kv{{"timeUnixNano": nanos(r.End), "severityText": sev, "severityNumber": num,
			"body": kv{"stringValue": string(r.Record)}, "attributes": attrs}}}}}}}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Exporter) tracesBody(r Round) kv {
	status := kv{"code": 1}
	if r.Error != "" {
		status = kv{"code": 2, "message": r.Error}
	}
	return kv{"resourceSpans": []kv{{"resource": e.resource(), "scopeSpans": []kv{{"scope": kv{"name": "ebac"},
		"spans": []kv{{"traceId": randHex(16), "spanId": randHex(8), "name": "ebac.round", "kind": 1,
			"startTimeUnixNano": nanos(r.Start), "endTimeUnixNano": nanos(r.End),
			"attributes": []kv{str("critic", r.Critic), str("source", r.Source)}, "status": status}}}}}}}
}

// metricsBody is every series this process holds for critic (cumulative
// counters since the process started, gauges at their last value).
func (e *Exporter) metricsBody(critic, source string) kv {
	now := nanos(e.now())
	var ms []kv
	keys := make([]string, 0, len(e.series))
	for k := range e.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	byName := map[string][]kv{}
	kinds := map[string]string{}
	var names []string
	for _, k := range keys {
		p := e.series[k]
		if p.critic != critic {
			continue
		}
		if _, seen := byName[p.name]; !seen {
			names = append(names, p.name)
		}
		attrs := []kv{str("critic", critic), str("source", source)}
		ak := make([]string, 0, len(p.attrs))
		for a := range p.attrs {
			ak = append(ak, a)
		}
		sort.Strings(ak)
		for _, a := range ak {
			attrs = append(attrs, str(a, p.attrs[a]))
		}
		dp := kv{"timeUnixNano": now, "asDouble": p.value, "attributes": attrs}
		if p.kind == "counter" {
			dp["startTimeUnixNano"] = nanos(e.start)
		}
		byName[p.name] = append(byName[p.name], dp)
		kinds[p.name] = p.kind
	}
	if len(names) == 0 {
		return nil
	}
	for _, n := range names {
		m := kv{"name": n}
		if kinds[n] == "counter" {
			m["sum"] = kv{"dataPoints": byName[n], "aggregationTemporality": 2, "isMonotonic": true}
		} else {
			m["gauge"] = kv{"dataPoints": byName[n]}
		}
		ms = append(ms, m)
	}
	return kv{"resourceMetrics": []kv{{"resource": e.resource(), "scopeMetrics": []kv{{"scope": kv{"name": "ebac"}, "metrics": ms}}}}}
}
