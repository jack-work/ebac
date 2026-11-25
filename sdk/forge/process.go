package forge

import (
	"context"
	"sync"
)

// One process, many GH values: a one-shot ebac verb may build several (one
// per host or per helper), and what it costs the shared GraphQL pool is their
// sum. Every charge a GH books is booked here too (#76: 75 comment-edit calls
// spent 156 points that no ledger saw).
var (
	processMu    sync.Mutex
	processSpend = Spend{Remaining: -1}
	processPacer Pacer
)

// ProcessSpent is what every GH of this process has spent so far.
func ProcessSpent() Spend {
	processMu.Lock()
	defer processMu.Unlock()
	return processSpend
}

// ResetProcessSpent zeroes the process total (tests).
func ResetProcessSpent() {
	processMu.Lock()
	processSpend = Spend{Remaining: -1}
	processMu.Unlock()
}

// Pacer spaces this process's GraphQL calls against a budget shared with
// other processes. Wait blocks until one more call fits and reserves the
// least a call costs (one point); it fails only when ctx ends. Settle books
// what the call cost beyond that reservation once GitHub has said.
type Pacer interface {
	Wait(ctx context.Context) error
	Settle(extra int)
}

// SetPacer makes p pace every GraphQL call of this process (nil: none) and
// returns a func that puts the previous one back.
func SetPacer(p Pacer) (restore func()) {
	processMu.Lock()
	prev := processPacer
	processPacer = p
	processMu.Unlock()
	return func() {
		processMu.Lock()
		processPacer = prev
		processMu.Unlock()
	}
}

func pacer() Pacer {
	processMu.Lock()
	defer processMu.Unlock()
	return processPacer
}

// paceCall waits for the process pacer, if any, before one GraphQL call.
func paceCall(ctx context.Context) error {
	if p := pacer(); p != nil {
		return p.Wait(ctx)
	}
	return nil
}

// settleCall books a call's cost beyond the one point Wait reserved.
func settleCall(points int) {
	if p := pacer(); p != nil && points > 1 {
		p.Settle(points - 1)
	}
}

// chargeProcess adds one charge to the process total: all counts every gh
// call, calls and points the GraphQL ones; remaining < 0 leaves the last
// answer in place.
func chargeProcess(all, calls, points, remaining int) {
	processMu.Lock()
	defer processMu.Unlock()
	processSpend.All += all
	processSpend.Calls += calls
	processSpend.Points += points
	if remaining >= 0 {
		processSpend.Remaining = remaining
	}
}

// chargeGraphQL books one dispatched GraphQL call at its least cost (one
// point) on this GH and on the process total.
func (g *GH) chargeGraphQL() {
	g.spendMu.Lock()
	g.spend.Calls++
	g.spend.Points++
	g.spendMu.Unlock()
	chargeProcess(0, 1, 1, -1)
}

// gqlWrite runs one GraphQL write (args after "api graphql ..." as given)
// on the paced, charged path: a write is one call of one point.
func (g *GH) gqlWrite(ctx context.Context, args []string) ([]byte, error) {
	if err := paceCall(ctx); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, args, nil)
	g.chargeGraphQL()
	return out, err
}
