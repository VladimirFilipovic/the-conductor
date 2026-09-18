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
	// MarkStaleHostsUnhealthy takes briefly-silent hosts out of scheduling
	// (fleet-wide, one statement) without touching their replicas; the next
	// heartbeat brings them back. Returns how many were demoted.
	MarkStaleHostsUnhealthy(ctx context.Context, lastHeartbeatBefore time.Time) (int64, error)
	// ListDeadHosts returns hosts silent past the death threshold that still
	// have bound replicas — the candidates for MarkHostDown.
	ListDeadHosts(ctx context.Context, lastHeartbeatBefore time.Time) ([]db.Host, error)
	// MarkHostDown takes the host out of scheduling AND frees its replicas
	// (hostless, unhealthy) in one atomic write, so the Reconciler's
	// anyHostlessReplicas sees them the very next snapshot. The write re-checks
	// last_heartbeat < lastHeartbeatBefore itself — a heartbeat that landed
	// after the sweep listed the host makes it a no-op.
	MarkHostDown(ctx context.Context, hostID uuid.UUID, lastHeartbeatBefore time.Time) error
	// CompleteDrainedHosts cordons every draining host that has no stateless
	// replica left and returns them. The drain's end is decided here, on
	// observed rows, not by the reconciler: it keys on reap, not on drain.
	CompleteDrainedHosts(ctx context.Context) ([]db.Host, error)
	// ListStalledDrains returns hosts draining since before startedBefore —
	// visibility only, nothing acts on them.
	ListStalledDrains(ctx context.Context, startedBefore time.Time) ([]db.Host, error)
	// OldestLiveApiserverStart reports when the longest-running apiserver that
	// has heartbeated since heartbeatAfter started; ok=false means none has.
	OldestLiveApiserverStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error)
}

// watchdogInterval is the idle gap between stale-host sweeps (same
// level-triggered shape as the engine's reconcileInterval).
const watchdogInterval = 5 * time.Second

// Two staleness thresholds, split by cost of the consequence (k8s-style
// notready vs eviction):
//
// hostUnhealthyAfter is how long a host may go silent before it stops
// receiving NEW work. Cheap and reversible — the next heartbeat flips it
// back to healthy — so it can sit close to the heartbeat cadence; a blip
// costs nothing but a few skipped placements.
const hostUnhealthyAfter = 30 * time.Second

// hostDeadAfter is how long a host may go silent before its replicas are
// freed for re-placement. Expensive and one-way (restarts, stateful
// failover), so it gets a much longer fuse: only a host dead beyond
// reasonable doubt pays it.
const hostDeadAfter = 2 * time.Minute

// drainStalledAfter is how long a drain may run before the sweep starts
// warning about it. A healthy evacuation is a canary, a batch and a drain
// window — minutes, not tens of minutes; past this something is stuck (no
// capacity in the region, a replacement that never turns healthy) and a
// human should look. No automatic action: the drain keeps waiting.
const drainStalledAfter = 10 * time.Minute

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
// have their replicas freed for the Reconciler to re-place. Drains settle in
// the same sweep: they too are observed-state verdicts on the host row.
func (s *Watchdog) sweepStaleHosts(ctx context.Context) error {
	now := s.now()
	demoted, err := s.store.MarkStaleHostsUnhealthy(ctx, now.Add(-hostUnhealthyAfter))
	if err != nil {
		return fmt.Errorf("mark stale hosts unhealthy: %w", err)
	}
	if demoted > 0 {
		slog.Info("watchdog -> stale hosts out of scheduling", "count", demoted)
	}
	if err := s.settleDrains(ctx, now); err != nil {
		return err
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

// settleDrains closes every drain whose stateless replicas are gone and warns
// about the ones that have run past drainStalledAfter. Independent of the
// death verdict's fairness: a drain's end is read off replica rows, not off
// heartbeat timestamps, so an apiserver outage doesn't make it unfair.
func (s *Watchdog) settleDrains(ctx context.Context, now time.Time) error {
	done, err := s.store.CompleteDrainedHosts(ctx)
	if err != nil {
		return fmt.Errorf("complete drained hosts: %w", err)
	}
	for _, h := range done {
		slog.Info("watchdog -> drain complete, host cordoned",
			"host", h.ID, "hostname", h.Hostname, "region", h.Region)
	}
	stalled, err := s.store.ListStalledDrains(ctx, now.Add(-drainStalledAfter))
	if err != nil {
		return fmt.Errorf("list stalled drains: %w", err)
	}
	for _, h := range stalled {
		slog.Warn("watchdog -> drain stalled, still holding stateless replicas",
			"host", h.ID, "hostname", h.Hostname, "region", h.Region,
			"draining_for", now.Sub(h.DrainStartedAt.Time).Round(time.Second))
	}
	return nil
}

// deathVerdictFair is the startup grace, keyed on the apiserver rather than on
// this process: a heartbeat can only be stale by a host's fault if some
// apiserver has been continuously live for a full death window. After an
// apiserver outage every last_heartbeat is stale by the plane's own absence,
// and agents deserve that window to reconnect before their replicas are freed.
// No live apiserver at all means nobody could have heartbeated — never fair.
func (s *Watchdog) deathVerdictFair(ctx context.Context, now time.Time) (bool, error) {
	started, ok, err := s.store.OldestLiveApiserverStart(ctx, now.Add(-domain.ApiserverLivenessWindow))
	if err != nil {
		return false, fmt.Errorf("apiserver liveness: %w", err)
	}
	return ok && now.Sub(started) >= hostDeadAfter, nil
}
