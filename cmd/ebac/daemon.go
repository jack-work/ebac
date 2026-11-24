package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gluck/ebac/internal/budget"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/fleet"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/otel"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/sched"
	"github.com/gluck/ebac/internal/systemd"
	"github.com/gluck/ebac/sdk/model"
)

// ---------- daemon (ebacd) ----------

// DaemonConfig is ebacd's configuration file (default /etc/ebac/config.json,
// rendered by the system flake from step 3 of the rework; until then absent,
// and the defaults below apply).
type DaemonConfig struct {
	Budget budget.Config `json:"budget"`
}

// DefaultDaemonConfig: ebac's share of the GHE account (ebac-steward 11-19):
// GraphQL 40 points/h, burst 10, every GitHub round defers while the account
// has under 1000 points left; REST 600 calls/h, burst 60.
func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{Budget: budget.Config{Credentials: map[string]map[string]budget.Limit{
		"github:microsoft.ghe.com": {
			"graphql": {PerHour: 40, Burst: 10, Floor: 1000},
			"rest":    {PerHour: 600, Burst: 60},
		},
	}}}
}

// loadDaemonConfig reads path; a missing file is the defaults, a bad one is
// an error (ebacd refuses to start on a config it cannot read).
func loadDaemonConfig(path string) (DaemonConfig, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultDaemonConfig(), nil
	}
	if err != nil {
		return DaemonConfig{}, err
	}
	var c DaemonConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return DaemonConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// MaxListFailures: ebacd exits non-zero after this many critic listings fail
// in a row (one a second), so Restart=always starts it afresh; a transport
// failure also drops the figaro connection at once so the next tick re-dials.
// 11-19: a figaro bounce closed ebacd's cached connection and every tick
// failed "connection closed" until a restart.
const MaxListFailures = 60

// figaroGuard re-dials figaro after a transport failure and counts listings
// that fail in a row.
type figaroGuard struct {
	reset func() error // drops the cached connections (figaro.Figaro.Close)
	fails int
}

// listed records one listing's outcome and says whether ebacd must exit.
func (g *figaroGuard) listed(err error) error {
	if err == nil {
		g.fails = 0
		return nil
	}
	g.fails++
	if figaro.Unavailable(err) {
		_ = g.reset()
	}
	if g.fails >= MaxListFailures {
		return fmt.Errorf("figaro unreachable for %d listings in a row (last: %v): exiting so the unit restarts", g.fails, err)
	}
	return nil
}

func cmdDaemon(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/ebac/config.json", "ebacd configuration (budgets); absent = the built-in defaults")
	pool := fs.Int("pool", sched.DefaultPool, "rounds run at once")
	timeout := fs.Duration("round-timeout", sched.DefaultRoundTimeout, "a round past this is killed and its pool slot released")
	_ = fs.Parse(args)

	cfg, err := loadDaemonConfig(*cfgPath)
	if err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	pl, err := newRunner(g)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	if retired, err := systemd.RetireCriticTimers(); err != nil {
		fmt.Printf("ebacd: retiring per-critic timers: %v (retired so far: %v)\n", err, retired)
	} else if len(retired) > 0 {
		fmt.Printf("ebacd: retired %d per-critic timer(s): %v\n", len(retired), retired)
	}
	ledger, err := budget.Open(cfg.Budget, filepath.Join(st.Root, "budget.json"), time.Now)
	if err != nil {
		fmt.Printf("ebacd: %v\n", err) // starts with empty buckets: it errs toward spending less
	}

	guard := &figaroGuard{reset: st.Fig.Close}
	var fatal error
	base := poll.Poller{Store: st, Fig: st.Fig, Plugins: pl, Out: os.Stdout, GH: g.gh, Host: g.host, BackoffDir: st.Root,
		OTel: otel.New(os.Getenv("EBAC_OTLP_ENDPOINT"))} // the node's agent, e.g. http://127.0.0.1:4318; unset exports nothing
	s := &sched.Scheduler{
		List: func(ctx context.Context) ([]*model.Critic, error) {
			ws, err := st.ListCritics(ctx)
			if f := guard.listed(err); f != nil {
				fatal = f
			}
			return ws, err
		},
		Run: func(ctx context.Context, name string, settle func([]model.Spend)) error {
			p := base // one Poller per round: OnSpend settles THIS round's grant
			p.OnSpend = settle
			return p.Poll(ctx, name)
		},
		End: func(ctx context.Context, w *model.Critic, reason string) error {
			p := base
			return p.End(ctx, w, reason)
		},
		Follow: func(ctx context.Context, id string) (string, bool, string, error) {
			return fleet.FollowRole(ctx, st.Fig.Form, id)
		},
		Repoint: func(ctx context.Context, w *model.Critic, from, to string) error {
			forms, err := fleet.ListForms(ctx, st.Fig)
			if err != nil {
				return err
			}
			_, err = fleet.Repoint(ctx, st, forms, fleet.RepointReq{From: from, To: to, Only: w.Name})
			return err
		},
		Warn: func(ctx context.Context, w *model.Critic, at time.Time) error {
			p := base
			return p.WarnEnd(ctx, w, at)
		},
		Ledger:       ledger,
		Now:          time.Now,
		Jitter:       func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d) + 1)) },
		Pool:         *pool,
		RoundTimeout: *timeout,
		Out:          os.Stdout,
	}
	fmt.Printf("ebacd: scheduling (pool %d, round deadline %s, budget %s)\n", *pool, *timeout, *cfgPath)

	beatID := ""
	beat := func() {
		b := sched.Beat{At: time.Now().UTC(), EveryS: int(sched.BeatEvery.Seconds()), Critics: s.Snapshot(), Buckets: ledger.Levels()}
		id, err := writeBeat(ctx, st.Fig, sched.BeatKind, "ebac-ebacd", beatID, b)
		if err != nil {
			fmt.Printf("ebacd: heartbeat: %v\n", err)
			return
		}
		beatID = id
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	hb := time.NewTicker(sched.BeatEvery)
	defer hb.Stop()
	s.Tick(ctx)
	beat()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("ebacd: stopping; waiting for running rounds\n")
			s.Wait()
			return nil
		case <-tick.C:
			s.Tick(ctx)
			if fatal != nil {
				fmt.Printf("ebacd: %v\n", fatal)
				stop()
				s.Wait()
				return fatal
			}
		case <-hb.C:
			beat()
		}
	}
}

// writeBeat writes the heartbeat to its form (kind ebac.daemon, minted once),
// finding it again if it is gone.
func writeBeat(ctx context.Context, fig *figaro.Figaro, kind, name, id string, b sched.Beat) (string, error) {
	if id == "" {
		found, err := fleet.FindFormsByKind(ctx, fig, kind)
		if err != nil {
			return "", err
		}
		switch len(found) {
		case 0:
			if id, err = fig.FormNew(ctx, name); err != nil {
				return "", err
			}
			if err := fig.SetJSON(ctx, id, "kind", kind); err != nil {
				return "", err
			}
		case 1:
			id = found[0]
		default:
			return "", fmt.Errorf("%d forms claim kind %s (%v): remove the stale ones", len(found), kind, found)
		}
	}
	if err := fig.SetJSON(ctx, id, "beat", b); err != nil {
		if errors.Is(err, figaro.ErrFormGone) {
			return "", err // the next beat finds or mints it again
		}
		return id, err
	}
	return id, nil
}
