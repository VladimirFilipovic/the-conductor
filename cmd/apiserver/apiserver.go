// Package apiserver is the `conductor apiserver` entrypoint: the agent-facing
// gRPC gateway and the operator-facing HTTP control plane in one process,
// separate from the engine so either side restarts without pausing the other.
package apiserver

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"conductor/internal/api"
	"conductor/internal/config"
	"conductor/internal/project"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

func Run(args []string) int {
	fs := flag.NewFlagSet("apiserver", flag.ContinueOnError)
	grpcAddr := fs.String("grpc.addr", ":7443", "agent gateway gRPC listen address")
	httpAddr := fs.String("http.addr", ":7080", "control-plane HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiserver: %v\n", err)
		return 1
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := storage.NewPostgresClient(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("apiserver: connect storage", "err", err)
		return 1
	}
	defer func() { _ = client.Close() }()

	// The gateway's LISTEN needs its own session-scoped connection, so it
	// dials the DSN directly instead of borrowing the pooled client.
	listen := func(ctx context.Context, onChange func(uuid.UUID), onReconnect func()) error {
		return storage.ListenReplicaChanges(ctx, cfg.DatabaseURL, onChange, onReconnect)
	}
	gateway := api.NewGateway(*grpcAddr, api.NewIngest(client), client, listen)
	// The control plane's desired-state writes run through the same project
	// service the CLI drives, so UI and CLI cannot disagree on the rules.
	server := api.NewServer(*httpAddr, gateway, api.NewControlPlane(client, project.New(client)), client)

	if err := server.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
