package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// ErrConflict is returned when a reconcile write loses its race: the row it
// targets moved between snapshot and commit (the Sensor advanced a replica, a
// live lease is held by another replica, a replica vanished). The caller
// should abandon this reconcile decision — the next tick recomputes from
// fresh state.
var ErrConflict = errors.New("conflict")

// ReplicaSpec is the desired shape of a replica the Reconciler places for a
// deployment in a region. The host is bound afterwards, once placement is
// decided; a stateful replica's volume binds at mint time (VolumeID zero for
// stateless).
type ReplicaSpec struct {
	DeploymentID  uuid.UUID
	Region        string
	CPUMillicores int32
	MemBytes      int64
	AllocReason   string
	VolumeID      uuid.UUID
}

// reconcileQuerier is the placement-write slice of Querier: the writes a
// reconcile pass commits. The guard each one carries (revision CAS,
// commit-time predicate, or none) is documented on engine.ReconcileTx, the
// consumer-side view that names why they belong in one transaction.
type reconcileQuerier interface {
	// ActiveVolumeLease re-checks the lease inside the tx to close the TOCTOU gap
	// before acquiring. ErrNotFound means the volume is free.
	ActiveVolumeLease(ctx context.Context, volumeID uuid.UUID) (db.VolumeLease, error)

	CreateReplica(ctx context.Context, spec ReplicaSpec) (db.Replica, error)
	// AssignReplicaHost reserves the replica onto the host via predicated
	// reservation: the UPDATE's WHERE re-checks "host ready, capacity still
	// sufficient" at commit time, so ErrConflict means the placement is
	// genuinely infeasible (or the replica vanished) — never a false abort.
	AssignReplicaHost(ctx context.Context, replicaID, hostID uuid.UUID) error

	AssignVolumeHost(ctx context.Context, volumeID, hostID uuid.UUID) error
	// MarkVolumeResizing approves a grow (attached → resizing); ErrConflict
	// when the volume is no longer attached-and-drifting.
	MarkVolumeResizing(ctx context.Context, volumeID uuid.UUID) error
	// MarkVolumeAttached settles a grow (resizing → attached); ErrConflict when
	// the volume isn't resizing or observed still trails desired.
	MarkVolumeAttached(ctx context.Context, volumeID uuid.UUID) error
	// AcquireVolumeLease takes (or renews) the single-writer lease; a live lease
	// held by a different replica returns ErrConflict.
	AcquireVolumeLease(ctx context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error
	SetReplicaDesiredStatus(ctx context.Context, replicaID uuid.UUID, desiredStatus domain.ReplicaDesiredStatus) error
	// SetReplicaPhase advances the lifecycle phase iff expectRevision still
	// matches; a moved revision (the Sensor wrote first) returns ErrConflict.
	SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase domain.ReplicaPhase, expectRevision int64) error
	// FreezeReplica parks a crash-looping replica of an active deployment in
	// failed. Phase-guarded rather than revision-CAS'd: the Sensor rewrites
	// the row every second, but the decision only depends on the monotonic
	// restart_count. ErrConflict when the row already left the live phases.
	FreezeReplica(ctx context.Context, replicaID uuid.UUID) error

	ReleaseVolumeLease(ctx context.Context, volumeID uuid.UUID) error
	DeleteReplica(ctx context.Context, replicaID uuid.UUID) error

	// SetDeploymentStatus flips a deployment's lifecycle status (failed on
	// crash-loop/stall, active on completion). Unconditional — the reconcile
	// path is the only status writer, so there is no race to guard.
	SetDeploymentStatus(ctx context.Context, deploymentID uuid.UUID, status domain.DeploymentStatus) error

	// SetServedRevision flips the slot's traffic switch to deploymentID. Must
	// share the tx with the outgoing drain batch so the blue/green shift is
	// atomic with retiring the old side.
	SetServedRevision(ctx context.Context, environmentServiceID uuid.UUID, region string, deploymentID uuid.UUID) error
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
		VolumeID:      uuid.NullUUID{UUID: spec.VolumeID, Valid: spec.VolumeID != uuid.Nil},
	})
	// The composite FK to deployment_regions rejects a (deployment, region) the
	// user never declared — surface it as a missing target, not a driver error.
	if fkViolation(err) {
		return db.Replica{}, fmt.Errorf("deployment %s region %q: %w", spec.DeploymentID, spec.Region, ErrNotFound)
	}
	return r, err
}

func (q querier) AssignReplicaHost(ctx context.Context, replicaID, hostID uuid.UUID) error {
	n, err := q.queries.ReserveReplicaOnHost(ctx, db.ReserveReplicaOnHostParams{
		ID:     replicaID,
		HostID: uuid.NullUUID{UUID: hostID, Valid: true},
	})
	if err != nil {
		return err
	}
	// Zero rows = the predicate rejected the reservation: the replica vanished,
	// the host left ready, or the capacity the placer saw is gone. Same
	// next-tick self-heal as a lost CAS.
	if n == 0 {
		slog.Debug("reconcile: reservation rejected, dropping placement", "replica", replicaID, "host", hostID)
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

func (q querier) MarkVolumeResizing(ctx context.Context, volumeID uuid.UUID) error {
	n, err := q.queries.MarkVolumeResizing(ctx, volumeID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

func (q querier) MarkVolumeAttached(ctx context.Context, volumeID uuid.UUID) error {
	n, err := q.queries.MarkVolumeAttached(ctx, volumeID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
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

func (q querier) SetReplicaDesiredStatus(ctx context.Context, replicaID uuid.UUID, desiredStatus domain.ReplicaDesiredStatus) error {
	return q.queries.SetReplicaDesiredStatus(ctx, db.SetReplicaDesiredStatusParams{
		ID:            replicaID,
		DesiredStatus: string(desiredStatus),
	})
}

func (q querier) SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase domain.ReplicaPhase, expectRevision int64) error {
	n, err := q.queries.SetReplicaPhase(ctx, db.SetReplicaPhaseParams{
		ID:       replicaID,
		Phase:    string(phase),
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

func (q querier) FreezeReplica(ctx context.Context, replicaID uuid.UUID) error {
	n, err := q.queries.FreezeReplica(ctx, replicaID)
	if err != nil {
		return err
	}
	if n == 0 {
		slog.Debug("reconcile: replica left live phases before freeze, dropping", "replica", replicaID)
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

func (q querier) SetDeploymentStatus(ctx context.Context, deploymentID uuid.UUID, status domain.DeploymentStatus) error {
	return q.queries.SetDeploymentStatus(ctx, db.SetDeploymentStatusParams{
		ID:     deploymentID,
		Status: string(status),
	})
}

func (q querier) SetServedRevision(ctx context.Context, environmentServiceID uuid.UUID, region string, deploymentID uuid.UUID) error {
	return q.queries.SetServedRevision(ctx, db.SetServedRevisionParams{
		EnvironmentServiceID: environmentServiceID,
		Region:               region,
		DeploymentID:         deploymentID,
	})
}

// nullString maps "" to a NULL text column; engine reasons (alloc_reason) are
// optional, so empty means "no reason recorded".
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
