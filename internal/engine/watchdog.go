package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type WatchdogStore interface {
	// MarkStaleHostsNotReady demotes briefly-silent ready hosts out of
	// scheduling (fleet-wide, one statement) without touching their replicas;
	// the next heartbeat promotes them back. Returns how many were demoted.
	MarkStaleHostsNotReady(ctx context.Context, lastHeartbeatBefore time.Time) (int64, error)
	// ListDeadHosts returns hosts silent past the death threshold that still
	// have bound replicas — the candidates for MarkHostDown.
	ListDeadHosts(ctx context.Context, lastHeartbeatBefore time.Time) ([]db.Host, error)
	// MarkHostDown takes the host out of scheduling AND frees its replicas
	// (hostless, unhealthy) in one atomic write, so the Reconciler's
	// anyHostlessReplicas sees them the very next snapshot. The write re-checks
	// last_heartbeat < lastHeartbeatBefore itself — a heartbeat that landed
	// after the sweep listed the host makes it a no-op.
	MarkHostDown(ctx context.Context, hostID uuid.UUID, lastHeartbeatBefore time.Time) error
	// OldestLiveGatewayStart reports when the longest-running apiserver that
	// has heartbeated since heartbeatAfter started; ok=false means none has.
	OldestLiveGatewayStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error)
}

// watchdogInterval is the idle gap between stale-host sweeps (same
// level-triggered shape as the engine's reconcileInterval).
const watchdogInterval = 5 * time.Second

// Two staleness thresholds, split by cost of the consequence (k8s-style
// notready vs eviction):
//
// hostNotReadyAfter is how long a host may go silent before it stops
// receiving NEW work. Cheap and reversible — the next heartbeat flips it
// back to ready — so it can sit close to the heartbeat cadence; a blip
// costs nothing but a few skipped placements.
const hostNotReadyAfter = 30 * time.Second

// hostDeadAfter is how long a host may go silent before its replicas are
// freed for re-placement. Expensive and one-way (restarts, stateful
// failover), so it gets a much longer fuse: only a host dead beyond
// reasonable doubt pays it.
const hostDeadAfter = 2 * time.Minute

// Watchdog turns heartbeat silence into scheduling signal: its sweep demotes
// briefly-silent hosts and frees the replicas of hosts dead past doubt. The
// heartbeats themselves arrive through the apiserver's Ingest, not here — the
// watchdog only reads their timestamps. It never touches desired state; deciding
// is the Reconciler's job.
type Watchdog struct {
	store WatchdogStore
	// now is injectable so staleness tests control the clock.
	now func() time.Time
}

func NewWatchdog(store WatchdogStore) *Watchdog {
	return &Watchdog{store: store, now: time.Now}
}

// run sweeps for stale hosts until ctx ends. Blocking. A lone failed sweep is
// logged and retried — staleness only accrues, so a missed sweep is caught by
// the next; only a persistent fault takes the watchdog (and the engine) down.
func (s *Watchdog) run(ctx context.Context) error {
	timer := time.NewTimer(watchdogInterval)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("watchdog -> done")
			return nil
		case <-timer.C:
		}

		switch err := s.sweepStaleHosts(ctx); {
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			slog.Info("watchdog -> done")
			return nil
		default:
			failures++
			if failures >= maxConsecutiveFailures {
				return fmt.Errorf("watchdog: %d consecutive failed sweeps: %w", failures, err)
			}
			slog.Warn("watchdog: sweep failed, retrying next tick", "err", err, "consecutive", failures)
		}

		timer.Reset(watchdogInterval)
	}
}

// sweepStaleHosts applies the two staleness thresholds: briefly-silent hosts
// drop out of scheduling (reversible), and hosts silent past the death window
// have their replicas freed for the Reconciler to re-place.
func (s *Watchdog) sweepStaleHosts(ctx context.Context) error {
	now := s.now()
	demoted, err := s.store.MarkStaleHostsNotReady(ctx, now.Add(-hostNotReadyAfter))
	if err != nil {
		return fmt.Errorf("mark stale hosts notready: %w", err)
	}
	if demoted > 0 {
		slog.Info("watchdog -> stale hosts out of scheduling", "count", demoted)
	}

	fair, err := s.deathVerdictFair(ctx, now)
	if err != nil {
		return err
	}
	if !fair {
		return nil
	}
	deadCutoff := now.Add(-hostDeadAfter)
	dead, err := s.store.ListDeadHosts(ctx, deadCutoff)
	if err != nil {
		return fmt.Errorf("list dead hosts: %w", err)
	}
	for _, h := range dead {
		if err := s.store.MarkHostDown(ctx, h.ID, deadCutoff); err != nil {
			return fmt.Errorf("mark host %s down: %w", h.ID, err)
		}
		slog.Info("watchdog -> host down, replicas freed for re-placement",
			"host", h.ID, "hostname", h.Hostname, "region", h.Region,
			"last_heartbeat", h.LastHeartbeat.Time)
	}
	return nil
}

// deathVerdictFair is the startup grace, keyed on the apiserver rather than on
// this process: a heartbeat can only be stale by a host's fault if some
// gateway has been continuously live for a full death window. After an
// apiserver outage every last_heartbeat is stale by the plane's own absence,
// and agents deserve that window to reconnect before their replicas are freed.
// No live gateway at all means nobody could have heartbeated — never fair.
func (s *Watchdog) deathVerdictFair(ctx context.Context, now time.Time) (bool, error) {
	started, ok, err := s.store.OldestLiveGatewayStart(ctx, now.Add(-domain.GatewayLivenessWindow))
	if err != nil {
		return false, fmt.Errorf("gateway liveness: %w", err)
	}
	return ok && now.Sub(started) >= hostDeadAfter, nil
}
