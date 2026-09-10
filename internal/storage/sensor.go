package storage

import (
	"context"
	"database/sql"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// ReplicaObservation is one agent-reported replica state — the observed half
// the Sensor ingests. Lives here (like ReplicaSpec) so the engine's
// SensorStore contract and the Postgres implementation share the type without
// an import cycle.
type ReplicaObservation struct {
	ReplicaID      uuid.UUID
	Phase          string
	Healthy        bool
	RestartCount   int32
	LastExitReason string
}

func (q querier) RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time, status string) error {
	return q.queries.RecordHostHeartbeat(ctx, db.RecordHostHeartbeatParams{
		HostID:     hostID,
		ObservedAt: sql.NullTime{Time: observedAt, Valid: true},
		Status:     status,
	})
}

func (q querier) MarkStaleHostsNotReady(ctx context.Context, lastHeartbeatBefore time.Time) (int64, error) {
	return q.queries.MarkStaleHostsNotReady(ctx, sql.NullTime{Time: lastHeartbeatBefore, Valid: true})
}

func (q querier) ListDeadHosts(ctx context.Context, lastHeartbeatBefore time.Time) ([]db.Host, error) {
	return q.queries.ListDeadHosts(ctx, sql.NullTime{Time: lastHeartbeatBefore, Valid: true})
}

func (q querier) MarkHostDown(ctx context.Context, hostID uuid.UUID, lastHeartbeatBefore time.Time) error {
	return q.queries.MarkHostDown(ctx, db.MarkHostDownParams{
		HostID:              hostID,
		LastHeartbeatBefore: sql.NullTime{Time: lastHeartbeatBefore, Valid: true},
	})
}

// RecordReplicaObservation applies an agent report to the replica row. Zero
// rows affected means the observation lost to a terminal/orchestrator-owned
// phase — stale input, not an error, but the caller must know: a stale
// healthy report must not renew the volume lease.
func (q querier) RecordReplicaObservation(ctx context.Context, obs ReplicaObservation) (bool, error) {
	rows, err := q.queries.RecordReplicaObservation(ctx, db.RecordReplicaObservationParams{
		ReplicaID:      obs.ReplicaID,
		Phase:          obs.Phase,
		Healthy:        obs.Healthy,
		RestartCount:   obs.RestartCount,
		LastExitReason: nullString(obs.LastExitReason),
	})
	return rows > 0, err
}

func (q querier) ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error) {
	return q.queries.ListReplicasByHost(ctx, uuid.NullUUID{UUID: hostID, Valid: true})
}

func (q querier) RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error {
	return q.queries.RecordVolumeObservedSize(ctx, db.RecordVolumeObservedSizeParams{
		VolumeID:      volumeID,
		ObservedBytes: sql.NullInt64{Int64: observedBytes, Valid: true},
	})
}

// RenewVolumeLease extends the lease replicaID holds. Zero rows = no lease
// held by this replica (stateless, or the volume moved on) — a no-op, not an
// error, so the caller renews blindly on every healthy observation.
func (q querier) RenewVolumeLease(ctx context.Context, replicaID uuid.UUID, expiresAt time.Time) error {
	_, err := q.queries.RenewVolumeLease(ctx, db.RenewVolumeLeaseParams{
		ReplicaID: replicaID,
		ExpiresAt: expiresAt,
	})
	return err
}
