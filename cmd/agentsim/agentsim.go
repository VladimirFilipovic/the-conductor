// Package agentsim is the `conductor agentsim` entrypoint: the simulated
// host-agent fleet (one agent per host, chaos control API).
package agentsim

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"conductor/agentsim"
)

func Run(args []string) int {
	fs := flag.NewFlagSet("agentsim", flag.ContinueOnError)
	agentAPIAddr := fs.String("agentapi.addr", "localhost:7443", "apiserver AgentAPI gRPC address")
	controlAddr := fs.String("control.addr", ":7780", "chaos control API listen address")
	tick := fs.Duration("tick", time.Second, "agent tick: container progression + report cadence")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fleet := agentsim.NewFleet(*agentAPIAddr, *controlAddr, *tick)
	if err := fleet.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
