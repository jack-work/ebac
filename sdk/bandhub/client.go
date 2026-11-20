// Package bandhub is the band hub's read-only client (GET /stats, /nodes,
// /jobs): metadata only, never logs, outputs, artifacts or reasons. Shared by
// the ebac-bandhub plugin and the github plugin's scout.
package bandhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultSocket is where band-hub.nix puts the hub's socket.
const DefaultSocket = "/run/band-hub/sock"

// Stats is GET /stats, reduced to what the mirror renders.
type Stats struct {
	Hub struct {
		Now        int64   `json:"now"` // unix ms
		NodeStaleS float64 `json:"node_stale_s"`
	} `json:"hub"`
	Queues []QueueStats `json:"queues"`
}

type QueueStats struct {
	Queue  string `json:"queue"`
	Counts struct {
		Ready  int `json:"ready"`
		Leased int `json:"leased"`
	} `json:"counts"`
}

// Node is one row of GET /nodes.
type Node struct {
	Node     string  `json:"node"`
	AgeS     float64 `json:"age_s"`
	Received int64   `json:"received"` // unix ms of its last report
	Snapshot struct {
		Workers struct {
			Busy  int `json:"busy"`
			Total int `json:"total"`
		} `json:"workers"`
		Running []struct {
			HubJob int64 `json:"hub_job"`
		} `json:"running"`
		Disk struct {
			FreeBytes int64 `json:"free_bytes"`
			Guard     *struct {
				Tier    int  `json:"tier"`
				Drained bool `json:"drained"`
			} `json:"guard"`
		} `json:"disk"`
		Draining any `json:"draining"`
	} `json:"snapshot"`
}

// Job is one row of GET /jobs, reduced to metadata.
type Job struct {
	ID      int64  `json:"id"`
	Queue   string `json:"queue"`
	State   string `json:"state"`
	Created int64  `json:"created"`
	Payload struct {
		Template string                     `json:"template"`
		Params   map[string]json.RawMessage `json:"params"`
		// ParamsSum is the hub's digest of the FULL params (the metadata
		// view hides most of them): what dupes key on.
		ParamsSum string `json:"params_sum"`
	} `json:"payload"`
}

// Hub is the mirror's view of one poll.
type Hub struct {
	Stats Stats
	Nodes []Node
	Live  []Job // ready + leased
}

// Client speaks HTTP to the hub over its unix socket.
type Client struct {
	Socket  string
	Timeout time.Duration
}

func (c *Client) http() *http.Client {
	t := c.Timeout
	if t == 0 {
		t = 10 * time.Second
	}
	return &http.Client{Timeout: t, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
	}}
}

func (c *Client) get(ctx context.Context, cl *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://band-hub"+path, nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("band-hub %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("band-hub %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("band-hub %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("band-hub %s: %w", path, err)
	}
	return nil
}

// Get reads one metadata route into v (a metaReaders principal sees only what
// the hub's meta view allows).
func (c *Client) Get(ctx context.Context, path string, v any) error {
	return c.get(ctx, c.http(), path, v)
}

// Fetch reads the hub's metadata: stats, nodes, ready and leased jobs.
func (c *Client) Fetch(ctx context.Context) (*Hub, error) {
	cl := c.http()
	h := &Hub{}
	if err := c.get(ctx, cl, "/stats", &h.Stats); err != nil {
		return nil, err
	}
	if err := c.get(ctx, cl, "/nodes", &h.Nodes); err != nil {
		return nil, err
	}
	for _, st := range []string{"ready", "leased"} {
		var jobs []Job
		if err := c.get(ctx, cl, "/jobs?state="+st+"&sort=id&order=asc&limit=1000", &jobs); err != nil {
			return nil, err
		}
		h.Live = append(h.Live, jobs...)
	}
	return h, nil
}
