package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jack-work/figaro/api/message"
	"github.com/jack-work/figaro/api/rpc"
	"github.com/jack-work/figaro/api/transport"
	"github.com/jack-work/figaro/sdk"
)

// Figaro talks to the angelus over its socket, not by forking the CLI.
//
// What this buys, in order of how much it matters:
//
//  1. VERSIONED WRITES. `Set(patch, ifVersion)` refuses a write whose base
//     moved -- "form moved: at version 3, not 2: re-read and retry". Shelling
//     out to `figaro set` has no such gate, so a poll round and the reconciler
//     writing one form clobbered each other silently. Now they cannot.
//  2. Typed responses instead of hand-rolled structs guessed from -j output.
//     (The bug that motivated this: ListResponse's field is Figaros, not
//     Arias, and only the compiler was ever going to say so.)
//  3. No process spawn per call. A poll round made a dozen.
//
// One thing it does NOT buy, measured rather than assumed: role redirection
// is implemented in the CLI, not the daemon. `figaro.qua` against a role's
// own endpoint is refused -- "@x is a form, not a figaro: figaro.qua needs a
// turn loop and a form has none". So Send resolves target-aria itself, per
// call, which preserves late binding exactly as the CLI does it.
type Figaro struct {
	sock    string
	Timeout time.Duration

	mu    sync.Mutex
	ang   *sdk.Angelus
	nodes map[string]*sdk.Aria
}

// NewFigaro takes a socket path. An empty string discovers it.
func NewFigaro(sock string) *Figaro {
	if sock == "" {
		sock = AngelusSocket()
	}
	return &Figaro{sock: sock, Timeout: 60 * time.Second, nodes: map[string]*sdk.Aria{}}
}

// AngelusSocket finds the daemon. XDG_RUNTIME_DIR is the real source; the
// /run/user fallback exists because a systemd user unit does not always
// inherit it.
func AngelusSocket() string {
	if s := os.Getenv("FIGARO_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "figaro", "angelus.sock")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "figaro", "angelus.sock")
}

func (f *Figaro) angelus() (*sdk.Angelus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ang != nil {
		return f.ang, nil
	}
	a, err := sdk.DialAngelus(transport.UnixEndpoint(f.sock))
	if err != nil {
		return nil, fmt.Errorf("dial angelus at %s: %w (is the daemon running?)", f.sock, err)
	}
	f.ang = a
	return a, nil
}

func ept(e rpc.Endpoint) transport.Endpoint {
	return transport.Endpoint{Scheme: e.Scheme, Address: e.Address}
}

// node dials one aria or form, caching the connection for the process.
func (f *Figaro) node(ctx context.Context, id string) (*sdk.Aria, error) {
	f.mu.Lock()
	if c, ok := f.nodes[id]; ok {
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	ang, err := f.angelus()
	if err != nil {
		return nil, err
	}
	at, err := ang.Attach(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	c, err := sdk.DialAria(ept(at.Endpoint), nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", id, err)
	}
	f.mu.Lock()
	f.nodes[id] = c
	f.mu.Unlock()
	return c, nil
}

func (f *Figaro) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.nodes {
		_ = c.Close()
	}
	f.nodes = map[string]*sdk.Aria{}
	if f.ang != nil {
		err := f.ang.Close()
		f.ang = nil
		return err
	}
	return nil
}

// ---------- forms ----------

func (f *Figaro) FormNew(ctx context.Context, name string) (string, error) {
	ang, err := f.angelus()
	if err != nil {
		return "", err
	}
	nb, _ := json.Marshal(name)
	patch := &rpc.FormPatch{Set: map[string]json.RawMessage{"name": nb}}
	resp, err := ang.FormCreate(ctx, "", nil, patch)
	if err != nil {
		return "", fmt.Errorf("form create: %w", err)
	}
	// FormCreate hands back the endpoint, so seed the cache and skip an
	// Attach on the writes that always follow.
	if c, derr := sdk.DialAria(ept(resp.Endpoint), nil); derr == nil {
		f.mu.Lock()
		f.nodes[resp.FormID] = c
		f.mu.Unlock()
	}
	return resp.FormID, nil
}

// FormRaw returns the form's keys plus the version they were read at. The
// version is the whole point: it is the token a conditional write needs.
func (f *Figaro) FormRaw(ctx context.Context, id string) (map[string]json.RawMessage, uint64, error) {
	c, err := f.node(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.Form(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", id, err)
	}
	b, err := json.Marshal(resp.Snapshot)
	if err != nil {
		return nil, 0, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, 0, err
	}
	return m, resp.Version, nil
}

func (f *Figaro) Form(ctx context.Context, id string) (map[string]any, error) {
	raw, _, err := f.FormRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var a any
		if err := json.Unmarshal(v, &a); err != nil {
			continue
		}
		out[k] = a
	}
	return out, nil
}

// SetJSON writes ONE TOP-LEVEL key to a complete value, conditionally on the
// version it read.
//
// Two rules are enforced here rather than remembered:
//
//   - No dotted keys. Measured 2026-08-25: writing `k` to an object literal
//     and then setting `k.a` produces a FLAT sibling "k.a" beside the nested
//     one, and `unset k.a` then removes the decoy while the real value
//     stands.
//   - Conditional on version, with a bounded re-read-and-retry. A lost update
//     between the poller and the reconciler is silent, and silence is what
//     makes it expensive.
func (f *Figaro) SetJSON(ctx context.Context, id, key string, v any) error {
	if err := validFormKey(key); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	c, err := f.node(ctx, id)
	if err != nil {
		return err
	}
	// Attempts and jitter, both measured rather than guessed: at 4 attempts
	// with no backoff, 8 concurrent writers to one form LOST 4 of 8 keys
	// (live_test.go). Every loser re-read the same version and collided
	// again immediately, so the retries were synchronised rather than
	// spread. Jitter is what actually fixes it; more attempts alone only
	// moves the failure.
	const attempts = 12
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 0-2ms, growing, decorrelated per writer.
			d := time.Duration(rand.Int63n(int64(time.Millisecond)*2)) * time.Duration(i)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		cur, err := c.Form(ctx)
		if err != nil {
			return fmt.Errorf("read %s before write: %w", id, err)
		}
		patch := message.Patch{Set: map[string]json.RawMessage{key: json.RawMessage(b)}}
		if _, err := c.Set(ctx, patch, cur.Version); err != nil {
			if isVersionConflict(err) {
				// Somebody wrote between our read and our write. Re-read
				// and reapply: our key is whole, so replaying it on the
				// newer base is correct, not a merge.
				lastErr = err
				continue
			}
			return fmt.Errorf("set %s.%s: %w", id, key, err)
		}
		return nil
	}
	// Say what happened. Returning the raw conflict here reads as a single
	// collision rather than sustained contention, and sends the next reader
	// looking in the wrong place.
	return fmt.Errorf("set %s.%s: lost %d version races in a row (last: %v)", id, key, attempts, lastErr)
}

func (f *Figaro) Unset(ctx context.Context, id, key string) error {
	c, err := f.node(ctx, id)
	if err != nil {
		return err
	}
	cur, err := c.Form(ctx)
	if err != nil {
		return err
	}
	_, err = c.Set(ctx, message.Patch{Remove: []string{key}}, cur.Version)
	return err
}

func validFormKey(key string) error {
	for _, r := range key {
		if r == '.' {
			return fmt.Errorf("refusing dotted key %q: ebac writes top-level keys only "+
				"(a dotted write into a literal subtree creates a flat decoy)", key)
		}
	}
	return nil
}

// isVersionConflict recognises the daemon's refusal of a stale base:
// "form moved: at version 3, not 2: re-read and retry".
func isVersionConflict(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "form moved") ||
		strings.Contains(m, "if-version") ||
		strings.Contains(m, "version conflict")
}

func (f *Figaro) FormRemove(ctx context.Context, id string) error {
	ang, err := f.angelus()
	if err != nil {
		return err
	}
	f.mu.Lock()
	if c, ok := f.nodes[id]; ok {
		_ = c.Close()
		delete(f.nodes, id)
	}
	f.mu.Unlock()
	return ang.Kill(ctx, id, false)
}

// ---------- arias ----------

func (f *Figaro) NewAria(ctx context.Context, outfits string) (string, error) {
	ang, err := f.angelus()
	if err != nil {
		return "", err
	}
	var names []string
	if outfits != "" {
		names = splitComma(outfits)
	}
	resp, err := ang.Create(ctx, names, nil)
	if err != nil {
		return "", fmt.Errorf("create aria: %w", err)
	}
	if c, derr := sdk.DialAria(ept(resp.Endpoint), nil); derr == nil {
		f.mu.Lock()
		f.nodes[resp.FigaroID] = c
		f.mu.Unlock()
	}
	return resp.FigaroID, nil
}

// Send reaches whoever holds the role RIGHT NOW.
//
// target-aria is read here, per call, exactly as the CLI reads it, because
// the daemon does not redirect: a role's endpoint refuses figaro.qua. That
// per-call read IS the succession property -- two sends with a recast between
// them land on two different arias.
func (f *Figaro) Send(ctx context.Context, target, prompt string) error {
	aria := target
	if len(target) > 0 && target[0] == '@' {
		form, err := f.Form(ctx, target)
		if err != nil {
			return fmt.Errorf("resolve role %s: %w", target, err)
		}
		held, _ := form["target-aria"].(string)
		if held == "" {
			return fmt.Errorf("%s is a role with no target-aria: cast a figaro into it", target)
		}
		aria = held
	}
	return f.SendTo(ctx, aria, prompt)
}

func (f *Figaro) SendTo(ctx context.Context, ariaID, prompt string) error {
	c, err := f.node(ctx, ariaID)
	if err != nil {
		return err
	}
	// Qua submits and returns; it does not wait for the turn. That is the
	// fire-and-forget the heartbeat requires -- a poller that blocks on a
	// review turn is not a heartbeat.
	if _, _, err := c.Qua(ctx, prompt, nil); err != nil {
		return fmt.Errorf("send to %s: %w", ariaID, err)
	}
	return nil
}

func (f *Figaro) Cast(ctx context.Context, aria, formID string) error {
	c, err := f.node(ctx, aria)
	if err != nil {
		return err
	}
	if _, err := c.Cast(ctx, rpc.CastRequest{FormID: formID}); err != nil {
		return fmt.Errorf("cast %s into %s: %w", aria, formID, err)
	}
	return nil
}

// AriaExists reports whether an id still resolves. `dormant` means UNLOADED,
// not gone: collapsing a state into an absence is how a fleet audit once
// published nine live arias as reaped.
func (f *Figaro) AriaExists(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	ang, err := f.angelus()
	if err != nil {
		return false
	}
	if _, err := ang.Attach(ctx, id); err != nil {
		return false
	}
	return true
}

// AriaFill reports context fill as a fraction, plus the fields it came from.
// Never take a seat's word for this number.
func (f *Figaro) AriaFill(ctx context.Context, id string) (fill float64, tokens, limit int, err error) {
	ang, e := f.angelus()
	if e != nil {
		return 0, 0, 0, e
	}
	resp, e := ang.ListGlobal(ctx)
	if e != nil {
		return 0, 0, 0, e
	}
	found := false
	for _, r := range resp.Figaros {
		if r.ID == id {
			tokens, limit, found = r.ContextTokens, r.ContextLimit, true
			break
		}
	}
	if !found {
		return 0, 0, 0, fmt.Errorf("aria %s not found", id)
	}
	if limit <= 0 {
		// Unknown is not roomy. Reporting 0% for a seat that may be full
		// is the direction that loses the assignment.
		return 0, tokens, limit, fmt.Errorf("context_limit unknown for %s", id)
	}
	return float64(tokens) / float64(limit), tokens, limit, nil
}

func (f *Figaro) ForkAt(ctx context.Context, id string, turn int, outfits, prompt string) (string, error) {
	ang, err := f.angelus()
	if err != nil {
		return "", err
	}
	var names []string
	if outfits != "" {
		names = splitComma(outfits)
	}
	resp, err := ang.Fork(ctx, id, uint64(turn), 0, names, nil)
	if err != nil {
		return "", fmt.Errorf("fork %s at turn %d: %w", id, turn, err)
	}
	// fork KEEPS the parent's id and mints an ALTERNATIVE: the branch is
	// the alternative, not the continuation.
	child := resp.Alternative
	if child == "" {
		return "", fmt.Errorf("fork of %s returned no id", id)
	}
	if prompt != "" {
		if err := f.SendTo(ctx, child, prompt); err != nil {
			// The fork happened; report the partial rather than losing
			// the id, which would orphan a live aria.
			return child, fmt.Errorf("forked %s but could not brief it: %w", child, err)
		}
	}
	return child, nil
}

// ListFormIDs enumerates every unbound form the daemon knows about.
func (f *Figaro) ListFormIDs(ctx context.Context) ([]string, error) {
	ang, err := f.angelus()
	if err != nil {
		return nil, err
	}
	resp, err := ang.ListGlobal(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range resp.Figaros {
		if r.Kind == "form" && len(r.ID) > 0 && r.ID[0] == '@' {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// patchOf builds a one-key patch. Test helper kept beside the encoder it
// must agree with.
func patchOf(key string, v any) message.Patch {
	b, _ := json.Marshal(v)
	return message.Patch{Set: map[string]json.RawMessage{key: json.RawMessage(b)}}
}

// SetJSONIfVersion writes one key conditionally on an EXTERNALLY supplied
// version, and does not retry.
//
// This is the difference between a write and a CLAIM. SetJSON re-reads on
// conflict and reapplies, which is right for a projection whose value is
// whole and idempotent. A queue claim must NOT do that: re-reading would
// re-examine a queue that has changed, and the caller's decision about WHICH
// item to take was made against the version it quotes here. On conflict the
// caller re-reads the queue and decides again.
func (f *Figaro) SetJSONIfVersion(ctx context.Context, id, key string, v any, version uint64) error {
	if err := validFormKey(key); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	c, err := f.node(ctx, id)
	if err != nil {
		return err
	}
	patch := message.Patch{Set: map[string]json.RawMessage{key: json.RawMessage(b)}}
	if _, err := c.Set(ctx, patch, version); err != nil {
		return err
	}
	return nil
}
