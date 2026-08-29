package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// supervisorMaxRestarts is how many back-to-back crashes get a restart before
// the process gives up and exits (something outside — systemd, compose — owns
// recovery beyond that).
const supervisorMaxRestarts = 3

// supervisorStableAfter is how long a run must survive for its crash to count
// as fresh rather than chronic: a crash after a stable run gets a full restart
// budget again, so a fleet that hiccups once a day doesn't slowly spend its
// three lives.
const supervisorStableAfter = time.Minute

// Run drives the engine and sensor loops until ctx ends, restarting both after
// a crash. They restart together on purpose: they share the store, and a fault
// that killed one has usually poisoned the other's next pass anyway. No pacing
// between restarts — both inner loops already burn maxConsecutiveFailures
// ticks before returning an error, so this can't hot-loop.
func Run(ctx context.Context, e *Engine, sensor *Sensor) error {
	slog.Info("engine starting")
	defer slog.Info("engine shutting down")

	s := supervisor{maxRestarts: supervisorMaxRestarts, stableAfter: supervisorStableAfter, now: time.Now}
	return s.run(ctx, func(ctx context.Context) error {
		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error { return e.run(ctx) })
		g.Go(func() error { return sensor.run(ctx) })
		return g.Wait()
	})
}

// supervisor is the restart policy, split from Run so the budget arithmetic is
// testable with a fake clock and a fake run.
type supervisor struct {
	maxRestarts int
	stableAfter time.Duration
	now         func() time.Time
}

func (s supervisor) run(ctx context.Context, fn func(context.Context) error) error {
	restarts := 0
	for {
		started := s.now()
		err := fn(ctx)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if s.now().Sub(started) >= s.stableAfter {
			restarts = 0
		}
		restarts++
		if restarts > s.maxRestarts {
			return fmt.Errorf("engine: giving up after %d back-to-back crashes: %w", restarts, err)
		}
		slog.Error("engine crashed, restarting", "err", err, "restart", restarts, "budget", s.maxRestarts)
	}
}
