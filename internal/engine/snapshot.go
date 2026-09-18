package engine

import (
	"context"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// Engine-owned views of the snapshot rows: sqlc row types stop at this boundary
// so the Reconciler's inputs are plain values — testable, schema-rename-proof,
// domain enums instead of bare strings.

// desiredState is what the current deployment wants for one slot: the replica
// target plus the spec needed to mint a replica.
type desiredState struct {
	Slot          replicaSlot
	DeploymentID  uuid.UUID
	ServiceID     uuid.UUID
	Replicas      int32
	CPUMillicores int32
	MemBytes      int64
	// RestartMax caps a replica's restart_count before the crash guard trips the
	// deployment to failed; carried per group since it applies to the current deploy.
	RestartMax int32
	// ProgressDeadline (seconds) bounds how long the health gate may stay open
	// before the rollout trips to failed (stalled).
	ProgressDeadline int32
	// Status lets the crash guard skip an already-failed deploy (the notFailed
	// guard) so it holds instead of re-emitting deployFailed every tick.
	Status   domain.DeploymentStatus
	Stateful bool
}

type replica struct {
	ID           uuid.UUID
	DeploymentID uuid.UUID
	Slot         replicaSlot
	// HostID/VolumeID are zero when unplaced/unbound.
	HostID        uuid.UUID
	VolumeID      uuid.UUID
	CPUMillicores int32
	MemBytes      int64
	DesiredStatus domain.ReplicaDesiredStatus
	Phase         domain.ReplicaPhase
	Healthy       bool
	RestartCount  int32
	// DrainSeconds: reap once DrainedAt + DrainSeconds < now. Carried per replica
	// so an outgoing (superseded) replica keeps its own deployment's drain policy.
	DrainSeconds int32
	// DrainedAt is when the orchestrator drained this replica; zero until then.
	DrainedAt time.Time
	// HealthChecksPassedAt: zero = never passed a probe (the stalled-rollout
	// signal). Stamped once by a DB trigger, so it survives a later crash.
	HealthChecksPassedAt time.Time
	// CreatedAt is when the replica row was minted — the start reference the
	// progress-deadline gate measures against (observedAt − CreatedAt > deadline).
	CreatedAt time.Time
	Revision  int64
	Version   int32
	// Current mirrors the deployment's is_current: true for the revision to
	// converge toward, false for an outgoing revision to drain.
	Current bool
	// HostDraining: this replica leaves with its host. True only for a
	// stateless replica on a host the operator is draining — a volume-pinned
	// one cannot follow its host out (the volume stays), so the recreate
	// cascade must never see it as departing and surge a second writer.
	HostDraining bool
}

type host struct {
	ID            uuid.UUID
	Region        string
	CPUMillicores int32
	MemBytes      int64
	DiskBytes     int64
	// Open: operator status allows free placement. Every host in the snapshot
	// is healthy; a cordoned/draining one is here only so a volume-pinned
	// replica can return to it.
	Open bool
}

type volume struct {
	ID               uuid.UUID
	ServiceID        uuid.UUID
	Region           string
	HostID           uuid.UUID
	DesiredSizeBytes int64
	// ObservedSizeBytes is what the host's agent last reported on disk; 0
	// until the first report. desired > observed is the drift the resize pass
	// converges (grow-only).
	ObservedSizeBytes int64
	Status            domain.VolumeStatus
}

type stateSnapshot struct {
	observedAt time.Time
	desired    []desiredState
	replicas   []replica
	hosts      []host
	volumes    []volume
}

// SnapshotReader is the read side of a reconcile pass: the four whole-fleet
// queries whose results must come from one REPEATABLE READ snapshot, or the
// desired count scales out from under the replica list and tears the diff.
// Read-only; placement writes go through ReconcileTx.
type SnapshotReader interface {
	// SnapshotDesired returns one row per (current deployment, region): the
	// replica target plus the spec needed to mint a replica.
	SnapshotDesired(ctx context.Context) ([]db.SnapshotDesiredRow, error)
	// ListActiveReplicas returns the live fleet for services with a current
	// deployment — the observed half the diff compares against SnapshotDesired.
	// Includes replicas still under a superseded deployment (an in-flight
	// rollout); IsCurrent splits the new revision from the outgoing one.
	ListActiveReplicas(ctx context.Context) ([]db.ListActiveReplicasRow, error)
	// ListHealthyHosts returns every heartbeating host across all regions,
	// operator status included; the placer filters on status for free
	// placement and buckets by region for bin-packing.
	ListHealthyHosts(ctx context.Context) ([]db.Host, error)
	// ListActiveVolumes returns the disks of services with a current deployment,
	// keyed by (service_id, region) against the stateful rows of SnapshotDesired.
	ListActiveVolumes(ctx context.Context) ([]db.Volume, error)
}

// The tx-scoped Querier WithReadTx hands its callback covers this view.
var _ SnapshotReader = storage.Querier(nil)

func (e *Engine) loadSnapshot(ctx context.Context) (stateSnapshot, error) {
	var snap stateSnapshot
	err := e.store.WithReadTx(ctx, func(q storage.Querier) error {
		var err error
		snap, err = snapshotFrom(ctx, q)
		return err
	})
	if err != nil {
		return stateSnapshot{}, err
	}
	return snap, nil
}

// snapshotFrom is where the tx-scoped Querier narrows to the reads a pass is
// allowed to make.
func snapshotFrom(ctx context.Context, r SnapshotReader) (stateSnapshot, error) {
	desired, err := r.SnapshotDesired(ctx)
	if err != nil {
		return stateSnapshot{}, err
	}
	replicas, err := r.ListActiveReplicas(ctx)
	if err != nil {
		return stateSnapshot{}, err
	}
	hosts, err := r.ListHealthyHosts(ctx)
	if err != nil {
		return stateSnapshot{}, err
	}
	volumes, err := r.ListActiveVolumes(ctx)
	if err != nil {
		return stateSnapshot{}, err
	}
	return newStateSnapshot(desired, replicas, hosts, volumes), nil
}

func newStateSnapshot(
	desired []db.SnapshotDesiredRow,
	replicas []db.ListActiveReplicasRow,
	hosts []db.Host,
	volumes []db.Volume,
) stateSnapshot {
	snap := stateSnapshot{
		observedAt: time.Now(),
		desired:    make([]desiredState, 0, len(desired)),
		replicas:   make([]replica, 0, len(replicas)),
		hosts:      make([]host, 0, len(hosts)),
		volumes:    make([]volume, 0, len(volumes)),
	}
	for _, d := range desired {
		snap.desired = append(snap.desired, desiredState{
			Slot:             replicaSlot{d.EnvironmentServiceID, d.Region},
			DeploymentID:     d.DeploymentID,
			ServiceID:        d.ServiceID,
			Replicas:         d.DesiredReplicas,
			CPUMillicores:    d.CpuMillicores,
			MemBytes:         d.MemBytes,
			RestartMax:       d.RestartMax,
			ProgressDeadline: d.ProgressDeadline,
			Status:           domain.DeploymentStatus(d.Status),
			Stateful:         d.Stateful,
		})
	}
	for _, r := range replicas {
		snap.replicas = append(snap.replicas, replica{
			ID:                   r.ID,
			DeploymentID:         r.DeploymentID,
			Slot:                 replicaSlot{r.EnvironmentServiceID, r.Region},
			HostID:               r.HostID.UUID,
			VolumeID:             r.VolumeID.UUID,
			CPUMillicores:        r.CpuMillicores,
			MemBytes:             r.MemBytes,
			DesiredStatus:        domain.ReplicaDesiredStatus(r.DesiredStatus),
			Phase:                domain.ReplicaPhase(r.Phase),
			Healthy:              r.Healthy,
			RestartCount:         r.RestartCount,
			DrainSeconds:         r.DrainSeconds,
			DrainedAt:            r.DrainedAt.Time,
			HealthChecksPassedAt: r.HealthChecksPassedAt.Time,
			CreatedAt:            r.CreatedAt,
			Revision:             r.Revision,
			Version:              r.Version,
			Current:              r.IsCurrent,
			HostDraining:         r.HostDraining && !r.VolumeID.Valid,
		})
	}
	for _, h := range hosts {
		snap.hosts = append(snap.hosts, host{
			ID:            h.ID,
			Region:        h.Region,
			CPUMillicores: h.CpuMillicores,
			MemBytes:      h.MemBytes,
			DiskBytes:     h.DiskBytes,
			Open:          domain.HostStatus(h.Status) == domain.HostOpen,
		})
	}
	for _, v := range volumes {
		snap.volumes = append(snap.volumes, volume{
			ID:                v.ID,
			ServiceID:         v.ServiceID,
			Region:            v.Region,
			HostID:            v.HostID.UUID,
			DesiredSizeBytes:  v.DesiredSizeBytes,
			ObservedSizeBytes: v.ObservedSizeBytes.Int64,
			Status:            domain.VolumeStatus(v.Status),
		})
	}
	return snap
}
