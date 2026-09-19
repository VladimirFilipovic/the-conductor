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

// Addresses come from the environment so the image needs no start-command
// override to be pointed at a different apiserver; an explicit flag still wins.
// They live here rather than in internal/config because they are this command's
// server flags, not process-wide settings the CLI and engine also read.
const (
	varAgentAPIAddr = "CONDUCTOR_AGENTAPI_ADDR"
	varControlAddr  = "CONDUCTOR_CONTROL_ADDR"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Run(args []string) int {
	fs := flag.NewFlagSet("agentsim", flag.ContinueOnError)
	agentAPIAddr := fs.String("agentapi.addr", envOr(varAgentAPIAddr, "localhost:7443"),
		"apiserver AgentAPI gRPC address (env "+varAgentAPIAddr+")")
	controlAddr := fs.String("control.addr", envOr(varControlAddr, ":7780"),
		"chaos control API listen address (env "+varControlAddr+")")
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
