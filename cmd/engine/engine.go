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
	"conductor/internal/logbuf"
	"conductor/internal/storage"

	"golang.org/x/sync/errgroup"
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

	writers := []io.Writer{os.Stderr}

	// The ring tees off the same handler as stderr, so what the stream serves
	// is the log, not a second rendering of it.
	var logs *logbuf.Server
	if cfg.LogsAddr != "" {
		ring := logbuf.New(logbuf.DefaultCapacity)
		writers = append(writers, ring)
		logs = logbuf.NewServer(cfg.LogsAddr, ring)
	}

	if cfg.LogFile != "" {
		logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log file: %v\n", err)
			return 1
		}
		defer func() { _ = logFile.Close() }()
		writers = append(writers, logFile)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(
		io.MultiWriter(writers...),
		&slog.HandlerOptions{Level: cfg.LogLevel},
	)))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := storage.NewPostgresClient(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("engine: connect storage", "err", err)
		return 1
	}
	defer func() { _ = client.Close() }()

	watchdog := engine.NewWatchdog(client)
	eng := engine.New(client, engine.NewReconciler(*placement), engine.NewActuator(client))

	// The log stream shares the process but nothing else: it serves reads from
	// memory, so a reader can't slow the reconcile loop down, and a failed
	// listener still takes the container down for the supervisor to restart.
	g, ctx := errgroup.WithContext(ctx)
	if logs != nil {
		g.Go(func() error { return logs.Run(ctx) })
	}
	g.Go(func() error { return engine.Run(ctx, eng, watchdog) })

	if err := g.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}
