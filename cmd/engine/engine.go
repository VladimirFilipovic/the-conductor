package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"conductor/internal/config"
	"conductor/internal/engine"
	"conductor/internal/storage"
)

func Run(_ []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		return 1
	}

	logFile, err := os.OpenFile("engine.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log file: %v\n", err)
		return 1
	}
	defer logFile.Close()

	slog.SetDefault(slog.New(slog.NewTextHandler(
		io.MultiWriter(os.Stderr, logFile),
		&slog.HandlerOptions{Level: cfg.LogLevel},
	)))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := storage.NewPostgresClient(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("engine: connect storage", "err", err)
		return 1
	}
	defer client.Close()

	// Sensor still wires a zero store — its SensorStore methods aren't on the
	// Postgres client yet; it gets a real store once those land.
	sensor := engine.Sensor{}
	scheduler := engine.NewScheduler(client)

	if err := engine.Run(ctx, scheduler, &sensor); err != nil {
		fmt.Println(err)
		return 1
	}

	return 0
}
