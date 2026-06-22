package engine

import (
	"context"
	"log/slog"
)

func Run(ctx context.Context, scheduler *Scheduler, sensor *Sensor) error {
	slog.Info("engine starting")

	go sensor.tick(ctx)
	go scheduler.tick(ctx)

	<-ctx.Done()

	slog.Info("engine shutting down")
	return nil
}
