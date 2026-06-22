package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// ErrConflict is returned when an optimistic-concurrency write loses its race:
// a host's revision moved between bin-packing and reservation, or a live lease
// is already held by another replica. The caller should abandon this reconcile
// decision — the next tick recomputes from fresh state.
var ErrConflict = errors.New("conflict")

// ReplicaSpec is the desired shape of a replica the Scheduler places for a
// deployment in a region. Host and volume are bound afterwards, once placement
// is decided.
type ReplicaSpec struct {
	DeploymentID  uuid.UUID
	Region        string
	CPUMillicores int32
	MemBytes      int64
	AllocReason   string
}

// ReconcileTx is the set of writes that must commit together to keep the fleet's
// invariants intact: schedule+reserve (no orphan replica, no double-booked host)
// and the stateful single-writer lease. The reads that feed a placement decision
// run outside the tx, so this stays a short lock-holding window.
type ReconcileTx interface {
	// ActiveVolumeLease re-checks the lease inside the tx to close the TOCTOU gap
	// before acquiring. ErrNotFound means the volume is free.
	ActiveVolumeLease(ctx context.Context, volumeID uuid.UUID) (db.VolumeLease, error)

	CreateReplica(ctx context.Context, spec ReplicaSpec) (db.Replica, error)
	// AssignReplicaHost reserves the replica onto the host iff the host's revision
	// still matches expectHostRevision; a moved revision returns ErrConflict.
	AssignReplicaHost(ctx context.Context, replicaID, hostID uuid.UUID, expectHostRevision int64) error

	AssignVolumeHost(ctx context.Context, volumeID, hostID uuid.UUID) error
	// AcquireVolumeLease takes (or renews) the single-writer lease; a live lease
	// held by a different replica returns ErrConflict.
	AcquireVolumeLease(ctx context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error
	SetReplicaDesiredStatus(ctx context.Context, replicaID uuid.UUID, desiredStatus string) error
	// SetReplicaPhase advances the lifecycle phase iff expectRevision still
	// matches; a moved revision (the Sensor wrote first) returns ErrConflict.
	SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase string, expectRevision int64) error

	ReleaseVolumeLease(ctx context.Context, volumeID uuid.UUID) error
	DeleteReplica(ctx context.Context, replicaID uuid.UUID) error
}

// The tx-scoped querier handed to WithReconcileTx callbacks is exactly this view.
var _ ReconcileTx = querier{}

// WithReconcileTx runs fn against a tx-scoped ReconcileTx and commits if it
// returns nil. Any error — including an ErrConflict from a lost CAS — rolls the
// whole placement back, so the loop re-converges on the next tick.
func (c *PostgresClient) WithReconcileTx(ctx context.Context, fn func(ReconcileTx) error) error {
	return c.withTx(ctx, nil, func(q querier) error { return fn(q) })
}

func (q querier) ActiveVolumeLease(ctx context.Context, volumeID uuid.UUID) (db.VolumeLease, error) {
	lease, err := q.queries.ActiveVolumeLease(ctx, volumeID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.VolumeLease{}, ErrNotFound
	}
	return lease, err
}

func (q querier) CreateReplica(ctx context.Context, spec ReplicaSpec) (db.Replica, error) {
	r, err := q.queries.CreateReplica(ctx, db.CreateReplicaParams{
		DeploymentID:  spec.DeploymentID,
		Region:        spec.Region,
		CpuMillicores: spec.CPUMillicores,
		MemBytes:      spec.MemBytes,
		AllocReason:   nullString(spec.AllocReason),
	})
	// The composite FK to deployment_regions rejects a (deployment, region) the
	// user never declared — surface it as a missing target, not a driver error.
	if fkViolation(err) {
		return db.Replica{}, fmt.Errorf("deployment %s region %q: %w", spec.DeploymentID, spec.Region, ErrNotFound)
	}
	return r, err
}

func (q querier) AssignReplicaHost(ctx context.Context, replicaID, hostID uuid.UUID, expectHostRevision int64) error {
	n, err := q.queries.ReserveReplicaOnHost(ctx, db.ReserveReplicaOnHostParams{
		ID:       replicaID,
		HostID:   uuid.NullUUID{UUID: hostID, Valid: true},
		Revision: expectHostRevision,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		slog.Debug("reconcile: host revision moved, dropping placement", "replica", replicaID, "host", hostID)
		return ErrConflict
	}
	return nil
}

func (q querier) AssignVolumeHost(ctx context.Context, volumeID, hostID uuid.UUID) error {
	return q.queries.AssignVolumeHost(ctx, db.AssignVolumeHostParams{
		ID:     volumeID,
		HostID: uuid.NullUUID{UUID: hostID, Valid: true},
	})
}

func (q querier) AcquireVolumeLease(ctx context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error {
	n, err := q.queries.AcquireVolumeLease(ctx, db.AcquireVolumeLeaseParams{
		VolumeID:  volumeID,
		ReplicaID: replicaID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		slog.Debug("reconcile: volume lease contested", "volume", volumeID, "replica", replicaID)
		return ErrConflict
	}
	return nil
}

func (q querier) SetReplicaDesiredStatus(ctx context.Context, replicaID uuid.UUID, desiredStatus string) error {
	return q.queries.SetReplicaDesiredStatus(ctx, db.SetReplicaDesiredStatusParams{
		ID:            replicaID,
		DesiredStatus: desiredStatus,
	})
}

func (q querier) SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase string, expectRevision int64) error {
	n, err := q.queries.SetReplicaPhase(ctx, db.SetReplicaPhaseParams{
		ID:       replicaID,
		Phase:    phase,
		Revision: expectRevision,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		slog.Debug("reconcile: replica revision moved, dropping phase write", "replica", replicaID, "phase", phase)
		return ErrConflict
	}
	return nil
}

func (q querier) ReleaseVolumeLease(ctx context.Context, volumeID uuid.UUID) error {
	return q.queries.ReleaseVolumeLease(ctx, volumeID)
}

func (q querier) DeleteReplica(ctx context.Context, replicaID uuid.UUID) error {
	return q.queries.DeleteReplica(ctx, replicaID)
}

// nullString maps "" to a NULL text column; engine reasons (alloc_reason) are
// optional, so empty means "no reason recorded".
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
