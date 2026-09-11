// Package api is the apiserver process: two audiences, two transports, one
// store.
//
//   - AgentAPI (gRPC) faces host agents. Uplink reports flow into ObservedState;
//     downlink pushes each host its full replica state.
//   - OperatorAPI (HTTP) faces the UI and CLI. Reads assemble the fleet and
//     topology views; writes go through DesiredState (the project layer) or the
//     operator-only host/replica transitions.
//   - Server runs both side by side and keeps the process's own liveness row
//     fresh so the engine knows whether agents COULD have heartbeated.
//
// Wire naming: *JSON types are flat response shapes, *Node types are the
// nested topology tree, *Request types are inbound bodies.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"conductor/internal/domain"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// InstanceStore is the apiserver's own liveness record. The engine sensor
// reads it to know whether agents COULD have heartbeated (see
// gateway_instances in the schema).
type InstanceStore interface {
	RegisterGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error
	HeartbeatGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error
	DeregisterGatewayInstance(ctx context.Context, id uuid.UUID) error
	DeleteStaleGatewayInstances(ctx context.Context, heartbeatBefore time.Time) error
}

// instanceHeartbeatInterval refreshes the liveness row well inside
// domain.GatewayLivenessWindow, so one slow tick never reads as an outage.
const instanceHeartbeatInterval = 5 * time.Second

// Server is the apiserver process: AgentAPI (gRPC) and OperatorAPI (HTTP)
// side by side, plus the liveness heartbeat the engine's startup grace keys
// on. The two listeners share nothing but the store, so a fault in one is
// surfaced through the errgroup and takes the process down for the outer
// supervisor (compose, systemd) to restart.
type Server struct {
	httpAddr  string
	agents    *AgentAPI
	operators *OperatorAPI
	store     InstanceStore
	id        uuid.UUID
	now       func() time.Time
}

func NewServer(httpAddr string, agents *AgentAPI, operators *OperatorAPI, store InstanceStore) *Server {
	return &Server{httpAddr: httpAddr, agents: agents, operators: operators, store: store, id: uuid.New(), now: time.Now}
}

// Run blocks until ctx ends or one component fails.
func (s *Server) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.agents.Run(ctx) })
	g.Go(func() error { return s.serveHTTP(ctx) })
	g.Go(func() error { return s.livenessLoop(ctx) })
	return g.Wait()
}

func (s *Server) serveHTTP(ctx context.Context) error {
	srv := &http.Server{Addr: s.httpAddr, Handler: s.operators.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("operatorapi -> serving", "addr", s.httpAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("operatorapi: serve: %w", err)
	}
	return nil
}

// livenessLoop registers this instance (retrying while the schema is still
// being migrated by the engine container) and refreshes its heartbeat until
// ctx ends, deregistering on the way out so a clean stop doesn't leave a row
// the engine has to age out.
func (s *Server) livenessLoop(ctx context.Context) error {
	if err := s.registerWithRetry(ctx); err != nil {
		return err
	}
	defer func() {
		// The run ctx is done; deregister on a fresh short-lived one.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.DeregisterGatewayInstance(cleanup, s.id); err != nil {
			slog.Warn("apiserver: deregister failed; engine will age the row out", "err", err)
		}
	}()

	t := time.NewTicker(instanceHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := s.now()
		if err := s.store.HeartbeatGatewayInstance(ctx, s.id, now); err != nil {
			slog.Warn("apiserver: liveness heartbeat failed", "err", err)
			continue
		}
		if err := s.store.DeleteStaleGatewayInstances(ctx, now.Add(-domain.GatewayLivenessWindow)); err != nil {
			slog.Warn("apiserver: stale instance cleanup failed", "err", err)
		}
	}
}

// registerWithRetry returns ctx.Err() if ctx ends first, so the caller never
// proceeds to heartbeat (and later deregister) an instance that never existed.
func (s *Server) registerWithRetry(ctx context.Context) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for attempt := 1; ; attempt++ {
		err := s.store.RegisterGatewayInstance(ctx, s.id, s.now())
		if err == nil {
			slog.Info("apiserver -> registered", "instance", s.id)
			return nil
		}
		if attempt >= 30 {
			return fmt.Errorf("apiserver: register instance: %w", err)
		}
		slog.Warn("apiserver: register failed, retrying", "err", err, "attempt", attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
