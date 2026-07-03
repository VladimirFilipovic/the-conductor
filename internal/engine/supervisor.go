package engine

import (
	"context"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

// Run supervises the engine's components: the first one to fail cancels the
// rest and its error propagates to the caller.
// TODO: rerun up to 3 times on errors, restart the counter
func Run(ctx context.Context, e *Engine, sensor *Sensor) error {
	slog.Info("engine starting")
	defer slog.Info("engine shutting down")

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.run(ctx) })
	g.Go(func() error { return sensor.run(ctx) })
	return g.Wait()
}
