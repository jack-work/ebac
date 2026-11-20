package forge

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PullHeads reads refs/pull/<n>/head of each PR over git smart HTTP (protocol v2 ls-refs, one ref-prefix per PR): the
// head shas at no GraphQL quota (cheroot rule.ghe_quota). On a *.ghe.com host it asks api.<host>, the one GHE host the
// egress proxy passes, which serves git-upload-pack too (measured 2026-11-17). A PR missing from the answer is absent
// from the map.
func (g *GH) PullHeads(ctx context.Context, repo string, prs []int) (map[int]string, error) {
	if len(prs) == 0 {
		return map[int]string{}, nil
	}
	tok := ""
	if !g.NoCred {
		t, err := g.cred().Token(ctx)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	base := g.GitBase
	if base == "" {
		base = "https://" + gitHost(g.Host)
	}
	return lsPullHeads(ctx, g.httpClient(), base+"/"+repo+".git/git-upload-pack", tok, prs)
}

func gitHost(host string) string {
	if strings.HasSuffix(host, ".ghe.com") && !strings.HasPrefix(host, "api.") {
		return "api." + host
	}
	return host
}

func (g *GH) httpClient() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second} // the default transport honours HTTPS_PROXY (the egress proxy)
}

func pktLine(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%04x%s", len(s)+4, s)
}

func lsPullHeads(ctx context.Context, cl *http.Client, url, tok string, prs []int) (map[int]string, error) {
	var body bytes.Buffer
	pktLine(&body, "command=ls-refs\n")
	body.WriteString("0001")
	for _, n := range prs {
		pktLine(&body, fmt.Sprintf("ref-prefix refs/pull/%d/head\n", n))
	}
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Git-Protocol", "version=2")
	if tok != "" {
		req.SetBasicAuth("x-access-token", tok)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ls-refs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("ls-refs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return parseLsRefs(bufio.NewReader(io.LimitReader(resp.Body, 1<<20)))
}

// parseLsRefs reads "<oid> refs/pull/<n>/head" pkt-lines up to the flush packet.
func parseLsRefs(r *bufio.Reader) (map[int]string, error) {
	out := map[int]string{}
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, fmt.Errorf("ls-refs: answer cut short: %w", err)
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("ls-refs: bad pkt-line length %q", hdr[:])
		}
		if n == 0 {
			return out, nil
		}
		if n < 4 {
			return nil, fmt.Errorf("ls-refs: unexpected pkt-line %q", hdr[:])
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(r, line); err != nil {
			return nil, fmt.Errorf("ls-refs: answer cut short: %w", err)
		}
		if bytes.HasPrefix(line, []byte("ERR ")) {
			return nil, fmt.Errorf("ls-refs: %s", strings.TrimSpace(string(line[4:])))
		}
		oid, ref, ok := strings.Cut(strings.TrimSpace(string(line)), " ")
		num, found := strings.CutPrefix(ref, "refs/pull/")
		num, isHead := strings.CutSuffix(num, "/head")
		if !ok || !found || !isHead {
			continue
		}
		if k, err := strconv.Atoi(num); err == nil && len(oid) >= 40 {
			out[k] = oid
		}
	}
}
