// Package apiserver is the `conductor apiserver` entrypoint: the agent-facing
// gRPC AgentAPI and the operator-facing HTTP OperatorAPI in one process,
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
	grpcAddr := fs.String("grpc.addr", ":7443", "agent API gRPC listen address")
	httpAddr := fs.String("http.addr", ":7080", "operator API HTTP listen address")
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

	// The agent API's LISTEN needs its own session-scoped connection, so it
	// dials the DSN directly instead of borrowing the pooled client.
	listen := func(ctx context.Context, onChange func(uuid.UUID), onReconnect func()) error {
		return storage.ListenReplicaChanges(ctx, cfg.DatabaseURL, onChange, onReconnect)
	}
	agents := api.NewAgentAPI(*grpcAddr, api.NewObservedState(client), client, listen)
	// The operator API's desired-state writes run through the same project
	// service the CLI drives, so UI and CLI cannot disagree on the rules.
	operators := api.NewOperatorAPI(client, project.New(client))

	if err := api.NewServer(*httpAddr, agents, operators, client).Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
