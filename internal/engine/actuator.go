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

	"github.com/google/uuid"
)

type ActuatorStore interface {
	WithReconcileTx(ctx context.Context, fn func(storage.ReconcileTx) error) error
}

// volumeLeaseTTL bounds how long a dead engine can hold a volume hostage: the
// lease is (re-)acquired on every assign_host commit, and expiry frees the
// volume for the replacement replica. Renewal beyond placement is the host
// agent's job (todo #4).
const volumeLeaseTTL = 90 * time.Second

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
	err := a.store.WithReconcileTx(ctx, func(tx storage.ReconcileTx) error {
		return a.commit(ctx, tx, it)
	})
	return dropConflict(err, it)
}

// applySwitchBatch retires one slot's outgoing revision: every drain plus the
// served-revision flip commit together. A conflict on any drain (the Sensor
// moved a replica meanwhile) rolls the whole switch back — the pointer never
// moves off replicas that didn't actually start draining.
func (a *Actuator) applySwitchBatch(ctx context.Context, batch []Intent) error {
	err := a.store.WithReconcileTx(ctx, func(tx storage.ReconcileTx) error {
		for _, it := range batch {
			if err := tx.SetReplicaPhase(ctx, it.ReplicaID, domain.ReplicaPhaseDraining, it.Revision); err != nil {
				return err
			}
		}
		lead := batch[0]
		return tx.SetServedRevision(ctx, lead.Group.EnvironmentServiceID, lead.Group.Region, lead.DeploymentID)
	})
	return dropConflict(err, batch[0])
}

// commit maps one intent kind onto its tx calls.
func (a *Actuator) commit(ctx context.Context, tx storage.ReconcileTx, it Intent) error {
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
		return tx.AcquireVolumeLease(ctx, it.VolumeID, it.ReplicaID, a.now().Add(volumeLeaseTTL))

	case IntentPlaceVolume:
		return tx.AssignVolumeHost(ctx, it.VolumeID, it.HostID)

	case IntentDrain:
		return tx.SetReplicaPhase(ctx, it.ReplicaID, domain.ReplicaPhaseDraining, it.Revision)

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
			"kind", it.Kind, "replica", it.ReplicaID, "service", it.Group.EnvironmentServiceID)
		return nil
	}
	return fmt.Errorf("actuator: %s: %w", it.Kind, err)
}
