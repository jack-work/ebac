package main

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Promote is the queue's heartbeat: take pending PRs while under budget, turn
// each into a critic, stop at the ceiling.
//
// The budget is a count of LIVE CRITICS, not of seats or of tokens. Counting
// critics is honest about what it constrains -- concurrent assignments -- and
// it is predictable, which matters more here than precision: a queue that
// stalls because yesterday's review was expensive is a queue nobody can plan
// against. Spend is REPORTED beside the count so the ceiling can be tuned
// against evidence rather than feel.
//
// Slots free themselves. A critic archives when all its PRs close, so the
// loop closes without anybody sweeping it.
type Promoter struct {
	Store *Store
	Fig   *Figaro
	GH    *GH
	Out   io.Writer
	Host  string
	GHBin string
	Dry   bool
}

type PromoteResult struct {
	Active   int         `json:"active"`
	Max      int         `json:"max_active"`
	Pending  int         `json:"pending"`
	Promoted []string    `json:"promoted,omitempty"`
	Dropped  []string    `json:"dropped,omitempty"`
	Skipped  string      `json:"skipped,omitempty"`
	Items    []QueueItem `json:"-"`
}

func (p *Promoter) Promote(ctx context.Context) (*PromoteResult, error) {
	formID, err := QueueForm(ctx, p.Fig)
	if err != nil {
		return nil, err
	}

	critics, err := p.Store.ListCritics()
	if err != nil {
		return nil, err
	}
	res := &PromoteResult{}
	for _, c := range critics {
		if !c.Runtime.Archived {
			res.Active++
		}
	}

	// Each promotion changes the queue, so the version is re-read every
	// pass rather than carried across the loop.
	for {
		items, cfg, version, err := ReadQueue(ctx, p.Fig, formID)
		if err != nil {
			return nil, err
		}
		res.Max = cfg.MaxActive
		res.Pending = 0
		for _, it := range items {
			if it.State == QPending {
				res.Pending++
			}
		}
		res.Items = items

		if res.Active >= cfg.MaxActive {
			res.Skipped = fmt.Sprintf("at capacity: %d/%d active", res.Active, cfg.MaxActive)
			break
		}
		var next *QueueItem
		for i := range items {
			if items[i].State == QPending {
				next = &items[i]
				break // already sorted: priority DESC, then FIFO
			}
		}
		if next == nil {
			break
		}

		// Coverage can change while an item waits, so the enqueue-time
		// guard is not enough on its own.
		if owner := p.coveredBy(critics, next.PRKey()); owner != "" {
			next.State = QDropped
			next.Note = "already watched by critic " + owner
			if !p.Dry {
				_ = UpdateItem(ctx, p.Fig, formID, *next)
			}
			res.Dropped = append(res.Dropped, string(next.PRKey())+" (covered by "+owner+")")
			fmt.Fprintf(p.Out, "queue: dropped %s — %s\n", next.PRKey(), next.Note)
			continue
		}

		// A PR closed while it sat in the queue is not worth a seat.
		// Check before claiming, so a closed PR does not consume the slot
		// it would have held.
		if st, err := p.GH.FetchPR(ctx, next.Owner, next.Repo, next.Number); err == nil && st.Closed() {
			next.State = QDropped
			next.Note = "closed before promotion (" + st.State + ")"
			if !p.Dry {
				_ = UpdateItem(ctx, p.Fig, formID, *next)
			}
			res.Dropped = append(res.Dropped, string(next.PRKey())+" ("+st.State+")")
			fmt.Fprintf(p.Out, "queue: dropped %s — %s\n", next.PRKey(), next.Note)
			continue
		}

		if p.Dry {
			fmt.Fprintf(p.Out, "queue: DRY RUN would promote %s (priority %d)\n", next.PRKey(), next.Priority)
			res.Promoted = append(res.Promoted, string(next.PRKey()))
			res.Active++
			// Do not loop forever on an unclaimed item in dry run.
			break
		}

		// CLAIM, conditional on the version we decided against. If another
		// promoter got there first this is refused, and we re-read rather
		// than proceeding on a stale decision.
		if err := Claim(ctx, p.Fig, formID, *next, version); err != nil {
			if isVersionConflict(err) {
				fmt.Fprintf(p.Out, "queue: lost the race for %s, re-reading\n", next.PRKey())
				continue
			}
			return res, fmt.Errorf("claim %s: %w", next.PRKey(), err)
		}

		name, err := p.createCritic(ctx, cfg, *next)
		if err != nil {
			// The claim stands but the critic failed. Put it back rather
			// than leaving it claimed forever, and say why.
			back := *next
			back.State = QPending
			back.Note = "promotion failed: " + err.Error()
			_ = UpdateItem(ctx, p.Fig, formID, back)
			return res, fmt.Errorf("promote %s: %w", next.PRKey(), err)
		}

		done := *next
		done.State = QPromoted
		done.Critic = name
		done.Note = ""
		_ = UpdateItem(ctx, p.Fig, formID, done)

		res.Promoted = append(res.Promoted, string(next.PRKey())+" -> "+name)
		res.Active++
		fmt.Fprintf(p.Out, "queue: promoted %s -> critic %q (%d/%d active)\n",
			next.PRKey(), name, res.Active, cfg.MaxActive)
	}

	fmt.Fprintf(p.Out, "queue: %d/%d active, %d pending, %d promoted, %d dropped\n",
		res.Active, res.Max, res.Pending, len(res.Promoted), len(res.Dropped))
	if res.Skipped != "" {
		fmt.Fprintf(p.Out, "queue: %s\n", res.Skipped)
	}
	return res, nil
}

// createCritic is the queue's entrance to the same path `ebac add` uses.
func (p *Promoter) createCritic(ctx context.Context, cfg QueueConfig, it QueueItem) (string, error) {
	name := criticNameFor(it)
	if err := ValidName(name); err != nil {
		return "", err
	}
	if _, err := p.Store.LoadCritic(name); err == nil {
		return "", fmt.Errorf("critic %q already exists", name)
	}

	mode := Mode(cfg.Mode)
	if it.Mode != "" {
		mode = Mode(it.Mode)
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil {
		interval = 5 * time.Minute
	}

	c := &Critic{
		Name: name, Mode: mode, Host: p.Host, CreatedAt: time.Now().UTC(),
		GHBin: p.GHBin, IssueRepo: cfg.IssueRepo, Stop: DefaultStop(),
	}
	c.AddPR(it.Ref())

	formID, err := p.Fig.FormNew(ctx, "ebac-"+name)
	if err != nil {
		return "", fmt.Errorf("mint role: %w", err)
	}
	c.FormID = formID
	if err := p.Store.SaveCritic(c); err != nil {
		return "", err
	}

	seat, err := p.Fig.NewAria(ctx, cfg.Outfit)
	if err != nil {
		return "", fmt.Errorf("mint seat: %w", err)
	}
	if err := p.Fig.Cast(ctx, seat, formID); err != nil {
		return "", fmt.Errorf("cast %s into %s: %w", seat, formID, err)
	}

	poller := &Poller{Store: p.Store, GH: p.GH, Fig: p.Fig, Out: io.Discard}
	poller.projectAll(ctx, c, nil, &Delta{})

	// Adopt current state silently, so the first armed round reports the
	// delta since promotion rather than one wake per standing thread.
	poller.DryRun = true
	if err := poller.Poll(ctx, name); err != nil {
		fmt.Fprintf(p.Out, "queue: %s adopted with a warning: %v\n", name, err)
	}

	if err := EnableTimer(name, interval); err != nil {
		return name, fmt.Errorf("critic created but timer not armed: %w", err)
	}
	return name, nil
}

func (p *Promoter) coveredBy(critics []*Critic, key PRKey) string {
	for _, c := range critics {
		if c.Runtime.Archived {
			continue
		}
		for _, have := range c.PRs {
			if have.Key() == key {
				return c.Name
			}
		}
	}
	return ""
}

func criticNameFor(it QueueItem) string {
	return fmt.Sprintf("%s-%d", it.Repo, it.Number)
}
