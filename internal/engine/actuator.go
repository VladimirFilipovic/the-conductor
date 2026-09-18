package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// ReconcileTx is the set of writes that must commit together to keep the fleet's
// invariants intact: schedule+reserve (no orphan replica, no double-booked host)
// and the stateful single-writer lease. The reads that feed a placement decision
// run outside the tx, so this stays a short lock-holding window.
//
// Every write decided against a snapshot needs a guard matched to what can
// invalidate it, and each guard failure surfaces as storage.ErrConflict (drop,
// next tick re-decides):
//
//   - Revision CAS (SetReplicaPhase): the Sensor and the Reconciler write the
//     same replica row concurrently, and the decision (drain) depends on the
//     whole row — "revision unchanged" is the only check that catches ANY
//     interleaved write, including ABA (crash + restart lands back on the same
//     phase, but not the same revision).
//   - Commit-time predicate (AssignReplicaHost, AcquireVolumeLease, and the
//     three volume flips MarkVolumeResizePending / MarkVolumeResizing /
//     MarkVolumeAttached): the question isn't "did the row change" but "is
//     the decision still right" — capacity, host readiness, lease liveness,
//     resize drift or catch-up are re-checked in the UPDATE's WHERE, so
//     concurrent placements that all fit don't abort each other the way a
//     version proxy would.
//   - Phase guard (FreezeReplica): the decision depends on restart_count
//     alone, which only grows, so any interleaved Sensor write leaves it
//     valid — a revision CAS would only lose races (the Sensor bumps revision
//     every report). The WHERE just refuses to overwrite a phase another
//     owner already moved the row into (draining, replacing, terminal).
//   - Unguarded (create, destroy, status flips): create mints a fresh row,
//     destroy targets an already-terminal one, and deployment status has a
//     single writer — nothing can invalidate these between snapshot and commit.
type ReconcileTx interface {
	ActiveVolumeLease(ctx context.Context, volumeID uuid.UUID) (db.VolumeLease, error)
	CreateReplica(ctx context.Context, spec storage.ReplicaSpec) (db.Replica, error)
	AssignReplicaHost(ctx context.Context, replicaID, hostID uuid.UUID) error
	AssignVolumeHost(ctx context.Context, volumeID, hostID uuid.UUID) error
	MarkVolumeResizePending(ctx context.Context, volumeID uuid.UUID) error
	MarkVolumeResizing(ctx context.Context, volumeID uuid.UUID) error
	MarkVolumeAttached(ctx context.Context, volumeID uuid.UUID) error
	AcquireVolumeLease(ctx context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error
	SetReplicaDesiredStatus(ctx context.Context, replicaID uuid.UUID, desiredStatus domain.ReplicaDesiredStatus) error
	SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase domain.ReplicaPhase, expectRevision int64) error
	FreezeReplica(ctx context.Context, replicaID uuid.UUID) error
	ReleaseVolumeLease(ctx context.Context, volumeID uuid.UUID) error
	DeleteReplica(ctx context.Context, replicaID uuid.UUID) error
	SetDeploymentStatus(ctx context.Context, deploymentID uuid.UUID, status domain.DeploymentStatus) error
	SetServedRevision(ctx context.Context, environmentServiceID uuid.UUID, region string, deploymentID uuid.UUID) error
}

// The tx-scoped Querier WithTx hands its callback covers this view.
var _ ReconcileTx = storage.Querier(nil)

// ActuatorStore is the tx entry point every intent commits through; the
// callback narrows the Querier to ReconcileTx at the point of use.
type ActuatorStore interface {
	WithTx(ctx context.Context, fn func(storage.Querier) error) error
}

// Actuator commits intents to storage. It makes no decisions — it only applies
// what the Reconciler produced, losing CAS races safely (ErrConflict = another
// pass already moved the row; the next tick re-reads and self-heals).
type Actuator struct {
	store ActuatorStore
	// now stamps lease expiries; injectable so tests control the clock.
	now func() time.Time
}

func NewActuator(store ActuatorStore) *Actuator {
	return &Actuator{store: store, now: time.Now}
}

// Apply commits each intent in its own tx, so one lost race never rolls back
// unrelated work (all-or-nothing roughly doubles conflicts — Omega §5.2). The
// exception is a traffic-switch drain batch: retiring a superseded revision
// and flipping served_revisions must be one atomic unit per slot, or a tick
// could serve neither side or both.
func (a *Actuator) Apply(ctx context.Context, intents []Intent) error {
	type switchBatch struct {
		slot    replicaSlot
		intents []Intent
	}
	var batches []switchBatch
	for _, it := range intents {
		if it.Kind != IntentDrain || !it.SwitchTraffic {
			if err := a.applyOne(ctx, it); err != nil {
				return err
			}
			continue
		}
		i := slices.IndexFunc(batches, func(b switchBatch) bool { return b.slot == it.Group })
		if i < 0 {
			batches = append(batches, switchBatch{slot: it.Group})
			i = len(batches) - 1
		}
		batches[i].intents = append(batches[i].intents, it)
	}
	for _, b := range batches {
		if err := a.applySwitchBatch(ctx, b.intents); err != nil {
			return err
		}
	}
	return nil
}

func (a *Actuator) applyOne(ctx context.Context, it Intent) error {
	if it.Kind == IntentSkip {
		return nil
	}
	err := a.store.WithTx(ctx, func(q storage.Querier) error {
		return a.commit(ctx, q, it)
	})
	return dropConflict(err, it)
}

// applySwitchBatch retires one slot's outgoing revision: every drain plus the
// served-revision flip commit together. A conflict on any drain (the Sensor
// moved a replica meanwhile) rolls the whole switch back — the pointer never
// moves off replicas that didn't actually start draining.
func (a *Actuator) applySwitchBatch(ctx context.Context, batch []Intent) error {
	err := a.store.WithTx(ctx, func(q storage.Querier) error {
		return commitSwitchBatch(ctx, q, batch)
	})
	return dropConflict(err, batch[0])
}

func commitSwitchBatch(ctx context.Context, tx ReconcileTx, batch []Intent) error {
	for _, it := range batch {
		if err := tx.SetReplicaPhase(ctx, it.ReplicaID, domain.ReplicaPhaseDraining, it.Revision); err != nil {
			return err
		}
	}
	lead := batch[0]
	return tx.SetServedRevision(ctx, lead.Group.EnvironmentServiceID, lead.Group.Region, lead.DeploymentID)
}

// commit maps one intent kind onto its tx calls.
func (a *Actuator) commit(ctx context.Context, tx ReconcileTx, it Intent) error {
	switch it.Kind {
	case IntentCreate:
		// Hostless by design: the row lands with host_id NULL and next tick's
		// anyHostlessReplicas routes it through the placer (docs/bin-pack.md).
		_, err := tx.CreateReplica(ctx, storage.ReplicaSpec{
			DeploymentID:  it.DeploymentID,
			Region:        it.Group.Region,
			CPUMillicores: it.CPUMillicores,
			MemBytes:      it.MemBytes,
			VolumeID:      it.VolumeID,
		})
		return err

	case IntentAssignHost:
		if err := tx.AssignReplicaHost(ctx, it.ReplicaID, it.HostID); err != nil {
			return err
		}
		if it.VolumeID == uuid.Nil {
			return nil
		}
		// Stateful: the single-writer lease binds in the same tx as the host,
		// so a placed replica can never race another writer onto the volume.
		// The upsert's own predicate rejects a live foreign lease (ErrConflict).
		return tx.AcquireVolumeLease(ctx, it.VolumeID, it.ReplicaID, a.now().Add(domain.VolumeLeaseTTL))

	case IntentPlaceVolume:
		return tx.AssignVolumeHost(ctx, it.VolumeID, it.HostID)

	// One row each, and always on different ticks (the agent grows the disk
	// in between), so they never share a tx. The status flip is what the
	// downlink keys on: resizing unlocks the new desired size for the agent;
	// resize_pending is bookkeeping only — the agent keeps what it has.
	case IntentVolumeResizePending:
		return tx.MarkVolumeResizePending(ctx, it.VolumeID)

	case IntentResizeVolume:
		return tx.MarkVolumeResizing(ctx, it.VolumeID)

	case IntentVolumeResized:
		return tx.MarkVolumeAttached(ctx, it.VolumeID)

	case IntentDrain:
		return tx.SetReplicaPhase(ctx, it.ReplicaID, domain.ReplicaPhaseDraining, it.Revision)

	case IntentFreezeReplica:
		// The row's own UPDATE fires replicas_changed, so the host's agent
		// gets a HostState without this replica and tears the container down.
		return tx.FreezeReplica(ctx, it.ReplicaID)

	case IntentDestroy:
		if it.VolumeID != uuid.Nil {
			if err := tx.ReleaseVolumeLease(ctx, it.VolumeID); err != nil {
				return err
			}
		}
		return tx.DeleteReplica(ctx, it.ReplicaID)

	case IntentFail:
		return tx.SetDeploymentStatus(ctx, it.DeploymentID, domain.DeploymentFailed)

	case IntentComplete:
		if err := tx.SetDeploymentStatus(ctx, it.DeploymentID, domain.DeploymentActive); err != nil {
			return err
		}
		// Only a first deployment has no traffic-switch drain (no outgoing
		// side), so this is where its pointer lands; blue/green and recreate
		// re-assert the value their drain batch already committed.
		return tx.SetServedRevision(ctx, it.Group.EnvironmentServiceID, it.Group.Region, it.DeploymentID)
	}
	return fmt.Errorf("actuator: unknown intent kind %q", it.Kind)
}

// dropConflict turns a lost race into a non-event: the decision was made
// against state that moved, the tx rolled back, and the next tick recomputes
// from fresh state. Anything else is a real storage failure for the engine's
// failure counter.
func dropConflict(err error, it Intent) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrConflict) {
		slog.Debug("actuate -> intent lost its race, dropped",
			"kind", it.Kind, "replica", it.ReplicaID, "volume", it.VolumeID, "service", it.Group.EnvironmentServiceID)
		return nil
	}
	return fmt.Errorf("actuator: %s: %w", it.Kind, err)
}
