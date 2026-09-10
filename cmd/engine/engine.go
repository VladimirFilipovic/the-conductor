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

	"github.com/google/uuid"
)

func Run(args []string) int {
	fs := flag.NewFlagSet("engine", flag.ContinueOnError)
	placement := config.PlacementFlags(fs)
	grpcAddr := fs.String("grpc.addr", ":7443", "agent gateway gRPC listen address")
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

	sensor := engine.NewSensor(client)
	eng := engine.New(client, engine.NewReconciler(*placement), engine.NewActuator(client))
	// The gateway's LISTEN needs its own session-scoped connection, so it
	// dials the DSN directly instead of borrowing the pooled client.
	listen := func(ctx context.Context, onChange func(uuid.UUID), onReconnect func()) error {
		return storage.ListenReplicaChanges(ctx, cfg.DatabaseURL, onChange, onReconnect)
	}
	gateway := engine.NewGateway(*grpcAddr, sensor, client, listen)

	if err := engine.Run(ctx, eng, sensor, gateway); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}
