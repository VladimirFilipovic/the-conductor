package api

import (
	"context"
	"fmt"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

// ObservedStateStore is the observed-state write side agents feed through the
// AgentAPI. Consumer-side view: the concrete *storage.PostgresClient satisfies
// it; tests substitute fakes.
type ObservedStateStore interface {
	RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time) error
	// RecordReplicaObservation applies one agent report; false means the write
	// lost to a terminal/orchestrator-owned phase and was dropped as stale.
	RecordReplicaObservation(ctx context.Context, obs storage.ReplicaObservation) (bool, error)
	// RenewVolumeLease extends the lease replicaID holds; a no-op when it
	// holds none (stateless) or the lease moved to another replica.
	RenewVolumeLease(ctx context.Context, replicaID uuid.UUID, expiresAt time.Time) error
	// RecordVolumeObservedSize stores what the agent has on disk for a volume.
	RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error
}

// ObservedState is the counterpart of DesiredState: what agents report about
// the world, validated and clock-stamped here before it lands in the store. It
// never touches desired state and renders no verdicts — turning silence into
// scheduling signal is the engine sensor's sweep, deciding is the Reconciler.
type ObservedState struct {
	store ObservedStateStore
	// now is injectable so clock-stamping tests control time.
	now func() time.Time
}

func NewObservedState(store ObservedStateStore) *ObservedState {
	return &ObservedState{store: store, now: time.Now}
}

// WithClock swaps the clock (tests; the engine e2e harness drives a fake
// clock through both sides of the loop).
func (o *ObservedState) WithClock(now func() time.Time) *ObservedState {
	o.now = now
	return o
}

// RecordHeartbeat ingests a host agent's liveness ping. The observation time
// is stamped here, not taken from the agent — a skewed agent clock must not
// be able to keep a dead host looking alive. The beat carries nothing else:
// a host's health IS the beat, and its status belongs to the operator.
// TODO: agent auth — validation guards against buggy agents, not spoofed ones.
func (o *ObservedState) RecordHeartbeat(ctx context.Context, hostID uuid.UUID) error {
	return o.store.RecordHostHeartbeat(ctx, hostID, o.now())
}

// ObserveReplica ingests one agent-reported replica state. Stale reports
// (replica already draining/terminal) are dropped by the store, not errored.
// A healthy report doubles as the liveness proof that keeps the single-writer
// volume lease alive: renewal rides every healthy observation THAT LANDS —
// a report dropped as stale must not renew, or a partitioned zombie agent
// keeps the lease alive forever and the failover replica can never claim
// the volume.
func (o *ObservedState) ObserveReplica(ctx context.Context, obs storage.ReplicaObservation) error {
	if !domain.ReplicaPhase(obs.Phase).AgentReportable() {
		return fmt.Errorf("observedstate: replica phase %q is not agent-reportable", obs.Phase)
	}
	applied, err := o.store.RecordReplicaObservation(ctx, obs)
	if err != nil {
		return err
	}
	if !applied || !obs.Healthy {
		return nil
	}
	return o.store.RenewVolumeLease(ctx, obs.ReplicaID, o.now().Add(domain.VolumeLeaseTTL))
}

// ObserveVolumeSize ingests what an agent has on disk for one volume. No
// guard against a smaller-than-before report: observed state mirrors the disk,
// and a disk that shrank (recreated, corrupted) is exactly what the reconciler
// must see to re-approve the grow. A non-positive size is agent garbage — a
// disk that doesn't exist reports nothing, not zero.
func (o *ObservedState) ObserveVolumeSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error {
	if observedBytes <= 0 {
		return fmt.Errorf("observedstate: volume %s observed size %d is not positive", volumeID, observedBytes)
	}
	return o.store.RecordVolumeObservedSize(ctx, volumeID, observedBytes)
}
