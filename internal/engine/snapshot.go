package engine

import (
	"context"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// Engine-owned views of the snapshot rows. The sqlc row types stay confined to
// this boundary so the Reconciler's inputs are plain values — trivially
// constructable in tests, insulated from schema renames, and carrying domain
// enums instead of bare strings.

// desiredState is what the current deployment wants for one slot: the replica
// target plus the spec needed to mint a replica.
type desiredState struct {
	Slot          replicaSlot
	DeploymentID  uuid.UUID
	ServiceID     uuid.UUID
	Replicas      int32
	CPUMillicores int32
	MemBytes      int64
	Stateful      bool
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
	Revision      int64
	Version       int32
	// Current mirrors the deployment's is_current: true for the revision to
	// converge toward, false for an outgoing revision to drain.
	Current bool
}

type host struct {
	ID            uuid.UUID
	Region        string
	CPUMillicores int32
	MemBytes      int64
	DiskBytes     int64
	Revision      int64
}

type volume struct {
	ID               uuid.UUID
	ServiceID        uuid.UUID
	Region           string
	HostID           uuid.UUID
	DesiredSizeBytes int64
	Revision         int64
}

type stateSnapshot struct {
	desired  []desiredState
	replicas []replica
	hosts    []host
	volumes  []volume
}

func (e *Engine) loadSnapshot(ctx context.Context) (stateSnapshot, error) {
	var snap stateSnapshot
	err := e.store.WithReadTx(ctx, func(r storage.SnapshotReader) error {
		desired, err := r.SnapshotDesired(ctx)
		if err != nil {
			return err
		}
		replicas, err := r.ListActiveReplicas(ctx)
		if err != nil {
			return err
		}
		hosts, err := r.ListSchedulableHosts(ctx)
		if err != nil {
			return err
		}
		volumes, err := r.ListActiveVolumes(ctx)
		if err != nil {
			return err
		}
		snap = newStateSnapshot(desired, replicas, hosts, volumes)
		return nil
	})
	if err != nil {
		return stateSnapshot{}, err
	}
	return snap, nil
}

func newStateSnapshot(
	desired []db.SnapshotDesiredRow,
	replicas []db.ListActiveReplicasRow,
	hosts []db.Host,
	volumes []db.Volume,
) stateSnapshot {
	snap := stateSnapshot{
		desired:  make([]desiredState, 0, len(desired)),
		replicas: make([]replica, 0, len(replicas)),
		hosts:    make([]host, 0, len(hosts)),
		volumes:  make([]volume, 0, len(volumes)),
	}
	for _, d := range desired {
		snap.desired = append(snap.desired, desiredState{
			Slot:          replicaSlot{d.EnvironmentServiceID, d.Region},
			DeploymentID:  d.DeploymentID,
			ServiceID:     d.ServiceID,
			Replicas:      d.DesiredReplicas,
			CPUMillicores: d.CpuMillicores,
			MemBytes:      d.MemBytes,
			Stateful:      d.Stateful,
		})
	}
	for _, r := range replicas {
		snap.replicas = append(snap.replicas, replica{
			ID:            r.ID,
			DeploymentID:  r.DeploymentID,
			Slot:          replicaSlot{r.EnvironmentServiceID, r.Region},
			HostID:        r.HostID.UUID,
			VolumeID:      r.VolumeID.UUID,
			CPUMillicores: r.CpuMillicores,
			MemBytes:      r.MemBytes,
			DesiredStatus: domain.ReplicaDesiredStatus(r.DesiredStatus),
			Phase:         domain.ReplicaPhase(r.Phase),
			Healthy:       r.Healthy,
			RestartCount:  r.RestartCount,
			Revision:      r.Revision,
			Version:       r.Version,
			Current:       r.IsCurrent,
		})
	}
	for _, h := range hosts {
		snap.hosts = append(snap.hosts, host{
			ID:            h.ID,
			Region:        h.Region,
			CPUMillicores: h.CpuMillicores,
			MemBytes:      h.MemBytes,
			DiskBytes:     h.DiskBytes,
			Revision:      h.Revision,
		})
	}
	for _, v := range volumes {
		snap.volumes = append(snap.volumes, volume{
			ID:               v.ID,
			ServiceID:        v.ServiceID,
			Region:           v.Region,
			HostID:           v.HostID.UUID,
			DesiredSizeBytes: v.DesiredSizeBytes,
			Revision:         v.Revision,
		})
	}
	return snap
}
