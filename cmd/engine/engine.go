package engine

import (
	"context"
	"flag"
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

func Run(args []string) int {
	fs := flag.NewFlagSet("engine", flag.ContinueOnError)
	placement := config.PlacementFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		return 1
	}

	logW := io.Writer(os.Stderr)
	if cfg.LogFile != "" {
		logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log file: %v\n", err)
			return 1
		}
		defer logFile.Close()
		logW = io.MultiWriter(os.Stderr, logFile)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(
		logW,
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
	eng := engine.New(client, engine.NewReconciler(*placement), engine.NewActuator(client))

	if err := engine.Run(ctx, eng, &sensor); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}
