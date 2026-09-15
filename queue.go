package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// The queue is a FORM, not a file.
//
// A queue's one hard requirement is that two promoters cannot claim the same
// PR. A JSON file plus os.Rename gives last-writer-wins with no way to detect
// the loss; a form gives Set(patch, ifVersion), which refuses a write whose
// base moved. That is a real compare-and-swap, and it is the entire reason
// this is not queue.json.
//
// Two things fall out for free: every enqueue, claim and drop is a WAL record,
// so the queue has an audit log nobody had to write; and an overseer aria can
// study the queue and watch it drain.
//
// SHAPE: one top-level key per item (`q_<id>`), not one `items` array.
//   - each item stays under the 2046-char context elision limit, so a studying
//     aria sees every item whole rather than a cut list;
//   - only the changed item re-renders in a studied delta;
//   - no push or claim rewrites the whole queue.
//
// ORDERING is explicit -- priority DESC, then seq ASC -- because a JSON object
// has no order and iteration order is not one.

const (
	queueFormName = "ebac-queue"
	queueItemPfx  = "q_"
)

type QueueState string

const (
	QPending  QueueState = "pending"
	QClaimed  QueueState = "claimed"  // a promoter holds it; critic not yet created
	QPromoted QueueState = "promoted" // became a critic
	QDropped  QueueState = "dropped"  // closed before promotion, or removed
)

type QueueItem struct {
	ID       string     `json:"id"`
	URL      string     `json:"url"`
	Owner    string     `json:"owner"`
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	Priority int        `json:"priority"`
	Seq      int64      `json:"seq"`
	State    QueueState `json:"state"`
	Enqueued string     `json:"enqueued_at"`
	Claimed  string     `json:"claimed_at,omitempty"`
	Critic   string     `json:"critic,omitempty"`
	Mode     string     `json:"mode,omitempty"`
	Note     string     `json:"note,omitempty"`
}

func (q QueueItem) Key() string { return queueItemPfx + q.ID }
func (q QueueItem) Ref() PRRef {
	return PRRef{Owner: q.Owner, Repo: q.Repo, Number: q.Number, URL: q.URL}
}
func (q QueueItem) PRKey() PRKey { return MakePRKey(q.Owner, q.Repo, q.Number) }

type QueueConfig struct {
	MaxActive int    `json:"max_active"`
	Mode      string `json:"default_mode"`
	Outfit    string `json:"default_outfit"`
	IssueRepo string `json:"default_issue_repo,omitempty"`
	Interval  string `json:"critic_interval"`
}

func DefaultQueueConfig() QueueConfig {
	return QueueConfig{MaxActive: 3, Mode: string(ModeReview), Outfit: "sonnet", Interval: "5m"}
}

// ---------- the form ----------

// QueueForm finds the queue by PROPERTY -- the form carrying kind
// "ebac.queue" -- and mints it on first use.
//
// Deliberately NOT recorded in a file. Writing the queue's id into
// state/queue.json would put the one pointer that matters back on disk and
// undercut the reason the queue is a form at all. Discovery costs one
// ListGlobal plus a form read per form, at a cadence measured in minutes.
//
// Two queue forms is a real possibility (a lost race at first mint), so this
// refuses rather than picking one: silently choosing means half the enqueues
// land in a queue nobody drains.
func QueueForm(ctx context.Context, fig *Figaro) (string, error) {
	found, err := FindFormsByKind(ctx, fig, "ebac.queue")
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		id, err := fig.FormNew(ctx, queueFormName)
		if err != nil {
			return "", err
		}
		if err := fig.SetJSON(ctx, id, "kind", "ebac.queue"); err != nil {
			return "", err
		}
		if err := fig.SetJSON(ctx, id, "config", DefaultQueueConfig()); err != nil {
			return "", err
		}
		return id, nil
	default:
		return "", fmt.Errorf("%d forms claim kind ebac.queue (%s): resolve by hand, "+
			"or enqueues will split across queues", len(found), strings.Join(found, ", "))
	}
}

// FindFormsByKind returns every form declaring a kind. Detection by property,
// never by name: a form named "ebac-queue" that we did not write is not ours,
// and one somebody renamed still is.
func FindFormsByKind(ctx context.Context, fig *Figaro, kind string) ([]string, error) {
	ids, err := fig.ListFormIDs(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		f, err := fig.Form(ctx, id)
		if err != nil {
			continue
		}
		if k, _ := f["kind"].(string); k == kind {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ReadQueue returns every item plus the config and the version they were read
// at. The version is the CAS token: a claim must quote it back.
func ReadQueue(ctx context.Context, fig *Figaro, formID string) ([]QueueItem, QueueConfig, uint64, error) {
	raw, version, err := fig.FormRaw(ctx, formID)
	if err != nil {
		return nil, QueueConfig{}, 0, err
	}
	cfg := DefaultQueueConfig()
	if b, ok := raw["config"]; ok {
		_ = json.Unmarshal(b, &cfg)
	}
	var items []QueueItem
	for k, v := range raw {
		if !strings.HasPrefix(k, queueItemPfx) {
			continue
		}
		var it QueueItem
		if err := json.Unmarshal(v, &it); err != nil {
			// A single unparseable item must not hide the rest of the
			// queue. Surface it as a dropped row instead of vanishing.
			items = append(items, QueueItem{ID: strings.TrimPrefix(k, queueItemPfx),
				State: QDropped, Note: "unparseable: " + err.Error()})
			continue
		}
		items = append(items, it)
	}
	SortQueue(items)
	return items, cfg, version, nil
}

// SortQueue is the ordering, stated once: highest priority first, then FIFO
// by enqueue sequence. Every reader uses this; none of them may iterate a map
// and call the result an order.
func SortQueue(items []QueueItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		return items[i].Seq < items[j].Seq
	})
}

// Enqueue refuses a PR that is already covered.
//
// Two distinct duplicate checks, because they catch different mistakes and
// missing either one puts two seats on one pull request -- which is not just
// wasteful, it is two reviewers replying to the same human in the same
// thread. Measured the hard way: promoting #23650 and #23654 gave them
// critics of their own while the `aether` critic was already watching both.
func Enqueue(ctx context.Context, st *Store, fig *Figaro, formID, host string, ref PRRef, priority int, mode, note string) (QueueItem, error) {
	// 1. Already watched by a live critic?
	if critics, err := st.ListCritics(); err == nil {
		for _, c := range critics {
			if c.Runtime.Archived {
				continue
			}
			for _, have := range c.PRs {
				if have.Key() == ref.Key() {
					return QueueItem{}, fmt.Errorf("%s is already watched by critic %q; "+
						"queueing it would put two seats on one PR", ref.Key(), c.Name)
				}
			}
		}
	}
	// 2. Already in the queue?
	items, _, _, err := ReadQueue(ctx, fig, formID)
	if err != nil {
		return QueueItem{}, err
	}
	var maxSeq int64
	for _, it := range items {
		if it.Seq > maxSeq {
			maxSeq = it.Seq
		}
		// Idempotence: the same PR queued twice is almost always a mistake,
		// and promoting it twice would put two seats on one pull request.
		if it.PRKey() == ref.Key() && (it.State == QPending || it.State == QClaimed) {
			return it, fmt.Errorf("%s is already queued (%s, priority %d)", ref.Key(), it.State, it.Priority)
		}
	}
	it := QueueItem{
		ID: queueID(ref), URL: ref.URL, Owner: ref.Owner, Repo: ref.Repo, Number: ref.Number,
		Priority: priority, Seq: maxSeq + 1, State: QPending,
		Enqueued: time.Now().UTC().Format(time.RFC3339), Mode: mode, Note: note,
	}
	if it.URL == "" {
		// Use the configured host. This line hardcoded a literal, so --host and
		// EBAC_HOST were silently ignored on this path and every synthesised URL
		// pointed at the wrong forge.
		it.URL = fmt.Sprintf("https://%s/%s/%s/pull/%d", host, ref.Owner, ref.Repo, ref.Number)
	}
	return it, fig.SetJSON(ctx, formID, it.Key(), it)
}

func queueID(ref PRRef) string {
	s := fmt.Sprintf("%s_%s_%d", ref.Owner, ref.Repo, ref.Number)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// Claim moves one pending item to claimed, CONDITIONALLY on the form version
// it was read at. Two promoters racing: one wins, the other is refused and
// re-reads. This is the operation the whole design exists for.
func Claim(ctx context.Context, fig *Figaro, formID string, it QueueItem, atVersion uint64) error {
	it.State = QClaimed
	it.Claimed = time.Now().UTC().Format(time.RFC3339)
	return fig.SetJSONIfVersion(ctx, formID, it.Key(), it, atVersion)
}

func UpdateItem(ctx context.Context, fig *Figaro, formID string, it QueueItem) error {
	return fig.SetJSON(ctx, formID, it.Key(), it)
}

func RemoveItem(ctx context.Context, fig *Figaro, formID, id string) error {
	return fig.Unset(ctx, formID, queueItemPfx+id)
}

// ---------- CLI ----------

func cmdQueue(ctx context.Context, g globals, args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	st, err := NewStore()
	if err != nil {
		return err
	}
	fig := NewFigaro(g.figaro)
	defer fig.Close()
	formID, err := QueueForm(ctx, fig)
	if err != nil {
		return err
	}

	switch sub {
	case "add":
		fs := newFlagSet("queue add")
		var prs stringList
		fs.Var(&prs, "pr", "PR url or owner/repo#number (repeatable)")
		prio := fs.Int("priority", 0, "higher goes first; ties break FIFO")
		mode := fs.String("mode", "", "override the queue's default mode")
		note := fs.String("note", "", "why this is queued")
		_ = fs.Parse(args)
		if len(prs) == 0 {
			return fmt.Errorf("give at least one --pr")
		}
		for _, raw := range prs {
			ref, err := ParsePRRef(raw)
			if err != nil {
				return err
			}
			it, err := Enqueue(ctx, st, fig, formID, g.host, ref, *prio, *mode, *note)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ebac: %v\n", err)
				continue
			}
			fmt.Printf("queued %s (priority %d, seq %d)\n", it.PRKey(), it.Priority, it.Seq)
		}
		return nil

	case "rm":
		if len(args) == 0 {
			return fmt.Errorf("give an item id (ebac queue ls)")
		}
		if err := RemoveItem(ctx, fig, formID, args[0]); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[0])
		return nil

	case "config":
		fs := newFlagSet("queue config")
		max := fs.Int("max-active", -1, "ceiling on concurrent critics")
		mode := fs.String("mode", "", "default mode for promoted critics")
		outfit := fs.String("outfit", "", "default outfit for minted seats")
		issue := fs.String("issue-repo", "", "default issue repo")
		interval := fs.String("interval", "", "heartbeat for promoted critics")
		_ = fs.Parse(args)
		_, cfg, _, err := ReadQueue(ctx, fig, formID)
		if err != nil {
			return err
		}
		if *max >= 0 {
			cfg.MaxActive = *max
		}
		if *mode != "" {
			cfg.Mode = *mode
		}
		if *outfit != "" {
			cfg.Outfit = *outfit
		}
		if *issue != "" {
			cfg.IssueRepo = *issue
		}
		if *interval != "" {
			if _, err := time.ParseDuration(*interval); err != nil {
				return fmt.Errorf("--interval: %w", err)
			}
			cfg.Interval = *interval
		}
		if err := fig.SetJSON(ctx, formID, "config", cfg); err != nil {
			return err
		}
		dumpJSON(cfg)
		return nil

	case "promote":
		fs := newFlagSet("queue promote")
		dry := fs.Bool("dry-run", false, "decide, change nothing")
		_ = fs.Parse(args)
		ghBin, _ := resolveBin(g.gh)
		p := &Promoter{Store: st, Fig: fig, GH: NewGH(g.gh, g.host), Out: os.Stdout,
			Host: g.host, GHBin: ghBin, Dry: *dry}
		_, err := p.Promote(ctx)
		return err

	case "ls":
		fs := newFlagSet("queue ls")
		asJSON := fs.Bool("j", false, "JSON")
		_ = fs.Parse(args)
		items, cfg, version, err := ReadQueue(ctx, fig, formID)
		if err != nil {
			return err
		}
		critics, _ := st.ListCritics()
		active := 0
		for _, c := range critics {
			if !c.Runtime.Archived {
				active++
			}
		}
		if *asJSON {
			dumpJSON(map[string]any{"form": formID, "version": version,
				"config": cfg, "active": active, "items": items})
			return nil
		}
		fmt.Printf("queue %s (v%d) — %d/%d active critics\n\n", formID, version, active, cfg.MaxActive)
		if len(items) == 0 {
			fmt.Println("empty (ebac queue add --pr <url>)")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPR\tPRIO\tSEQ\tSTATE\tCRITIC\tNOTE")
		for _, it := range items {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
				it.ID, it.PRKey(), it.Priority, it.Seq, it.State, it.Critic, firstLine(it.Note))
		}
		tw.Flush()
		return nil
	}
	return fmt.Errorf("unknown: ebac queue %s (add|ls|rm|config|promote)", sub)
}
