package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type SensorStore interface {
	RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time, status string) error
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
	// RecordReplicaObservation applies one agent report; false means the write
	// lost to a terminal/orchestrator-owned phase and was dropped as stale.
	RecordReplicaObservation(ctx context.Context, obs storage.ReplicaObservation) (bool, error)
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)
	RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error
	// RenewVolumeLease extends the lease replicaID holds; a no-op when it
	// holds none (stateless) or the lease moved to another replica.
	RenewVolumeLease(ctx context.Context, replicaID uuid.UUID, expiresAt time.Time) error
}

// sensorSweepInterval is the idle gap between stale-host sweeps (same
// level-triggered shape as the engine's reconcileInterval).
const sensorSweepInterval = 5 * time.Second

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

// Sensor is the observed-state boundary: host agents push heartbeats and
// replica observations IN through it, and its sweep turns heartbeat silence
// into scheduling signal (host down → replicas freed for re-placement). It
// never touches desired state — deciding is the Reconciler's job.
type Sensor struct {
	store SensorStore
	// now is injectable so staleness tests control the clock.
	now func() time.Time
	// started is stamped by run(); while uptime is shorter than hostDeadAfter
	// the sweep skips the death pass — after a control-plane outage every
	// last_heartbeat is stale by our own absence, and agents deserve a full
	// death window to reconnect before their replicas are freed. Zero (direct
	// sweep calls in tests) disables the grace.
	started time.Time
}

func NewSensor(store SensorStore) *Sensor {
	return &Sensor{store: store, now: time.Now}
}

// RecordHeartbeat ingests a host agent's liveness ping. The observation time
// is stamped here, not taken from the agent — a skewed agent clock must not
// be able to keep a dead host looking alive.
// Only ready/notready are agent-reportable: cordoned/draining are
// operator-owned desired state, and any other value would trip the
// hosts.status CHECK — erroring every heartbeat until the live host is
// falsely swept as stale.
// TODO: agent auth — validation guards against buggy agents, not spoofed ones.
func (s *Sensor) RecordHeartbeat(ctx context.Context, hostID uuid.UUID, status string) error {
	if status != "ready" && status != "notready" {
		return fmt.Errorf("sensor: host status %q is not agent-reportable", status)
	}
	return s.store.RecordHostHeartbeat(ctx, hostID, s.now(), status)
}

// ObserveReplica ingests one agent-reported replica state. Stale reports
// (replica already draining/terminal) are dropped by the store, not errored.
// A healthy report doubles as the liveness proof that keeps the single-writer
// volume lease alive: renewal rides every healthy observation THAT LANDS —
// a report dropped as stale must not renew, or a partitioned zombie agent
// keeps the lease alive forever and the failover replica can never claim
// the volume.
func (s *Sensor) ObserveReplica(ctx context.Context, obs storage.ReplicaObservation) error {
	if !domain.ReplicaPhase(obs.Phase).AgentReportable() {
		return fmt.Errorf("sensor: replica phase %q is not agent-reportable", obs.Phase)
	}
	applied, err := s.store.RecordReplicaObservation(ctx, obs)
	if err != nil {
		return err
	}
	if !applied || !obs.Healthy {
		return nil
	}
	return s.store.RenewVolumeLease(ctx, obs.ReplicaID, s.now().Add(volumeLeaseTTL))
}

// ObserveVolumeSize records a volume's observed on-disk size (grow-only
// resize drift).
func (s *Sensor) ObserveVolumeSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error {
	return s.store.RecordVolumeObservedSize(ctx, volumeID, observedBytes)
}

// run sweeps for stale hosts until ctx ends. Blocking. A lone failed sweep is
// logged and retried — staleness only accrues, so a missed sweep is caught by
// the next; only a persistent fault takes the sensor (and the engine) down.
func (s *Sensor) run(ctx context.Context) error {
	s.started = s.now()
	timer := time.NewTimer(sensorSweepInterval)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("sensor -> done")
			return nil
		case <-timer.C:
		}

		switch err := s.sweepStaleHosts(ctx); {
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			slog.Info("sensor -> done")
			return nil
		default:
			failures++
			if failures >= maxConsecutiveFailures {
				return fmt.Errorf("sensor: %d consecutive failed sweeps: %w", failures, err)
			}
			slog.Warn("sensor: sweep failed, retrying next tick", "err", err, "consecutive", failures)
		}

		timer.Reset(sensorSweepInterval)
	}
}

// sweepStaleHosts applies the two staleness thresholds: briefly-silent hosts
// drop out of scheduling (reversible), and hosts silent past the death window
// have their replicas freed for the Reconciler to re-place.
func (s *Sensor) sweepStaleHosts(ctx context.Context) error {
	now := s.now()
	demoted, err := s.store.MarkStaleHostsNotReady(ctx, now.Add(-hostNotReadyAfter))
	if err != nil {
		return fmt.Errorf("mark stale hosts notready: %w", err)
	}
	if demoted > 0 {
		slog.Info("sensor -> stale hosts out of scheduling", "count", demoted)
	}

	if !s.started.IsZero() && now.Sub(s.started) < hostDeadAfter {
		return nil // startup grace: no death verdicts on heartbeats older than our own uptime
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
		slog.Info("sensor -> host down, replicas freed for re-placement",
			"host", h.ID, "hostname", h.Hostname, "region", h.Region,
			"last_heartbeat", h.LastHeartbeat.Time)
	}
	return nil
}
