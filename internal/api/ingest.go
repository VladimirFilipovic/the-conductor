package api

import (
	"context"
	"fmt"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

// IngestStore is the observed-state write side agents feed through the
// gateway. Consumer-side view: the concrete *storage.PostgresClient satisfies
// it; tests substitute fakes.
type IngestStore interface {
	RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time, status string) error
	// RecordReplicaObservation applies one agent report; false means the write
	// lost to a terminal/orchestrator-owned phase and was dropped as stale.
	RecordReplicaObservation(ctx context.Context, obs storage.ReplicaObservation) (bool, error)
	RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error
	// RenewVolumeLease extends the lease replicaID holds; a no-op when it
	// holds none (stateless) or the lease moved to another replica.
	RenewVolumeLease(ctx context.Context, replicaID uuid.UUID, expiresAt time.Time) error
}

// Ingest is the observed-state boundary agents push INTO: heartbeats and
// replica observations, validated and stamped here, land in the store. It
// never touches desired state and renders no verdicts — turning silence into
// scheduling signal is the engine sensor's sweep, deciding is the Reconciler.
type Ingest struct {
	store IngestStore
	// now is injectable so clock-stamping tests control time.
	now func() time.Time
}

func NewIngest(store IngestStore) *Ingest {
	return &Ingest{store: store, now: time.Now}
}

// WithClock swaps the ingest clock (tests; the engine e2e harness drives a
// fake clock through both sides of the loop).
func (i *Ingest) WithClock(now func() time.Time) *Ingest {
	i.now = now
	return i
}

// RecordHeartbeat ingests a host agent's liveness ping. The observation time
// is stamped here, not taken from the agent — a skewed agent clock must not
// be able to keep a dead host looking alive.
// Only ready/notready are agent-reportable: cordoned/draining are
// operator-owned desired state, and any other value would trip the
// hosts.status CHECK — erroring every heartbeat until the live host is
// falsely swept as stale.
// TODO: agent auth — validation guards against buggy agents, not spoofed ones.
func (i *Ingest) RecordHeartbeat(ctx context.Context, hostID uuid.UUID, status string) error {
	if status != "ready" && status != "notready" {
		return fmt.Errorf("ingest: host status %q is not agent-reportable", status)
	}
	return i.store.RecordHostHeartbeat(ctx, hostID, i.now(), status)
}

// ObserveReplica ingests one agent-reported replica state. Stale reports
// (replica already draining/terminal) are dropped by the store, not errored.
// A healthy report doubles as the liveness proof that keeps the single-writer
// volume lease alive: renewal rides every healthy observation THAT LANDS —
// a report dropped as stale must not renew, or a partitioned zombie agent
// keeps the lease alive forever and the failover replica can never claim
// the volume.
func (i *Ingest) ObserveReplica(ctx context.Context, obs storage.ReplicaObservation) error {
	if !domain.ReplicaPhase(obs.Phase).AgentReportable() {
		return fmt.Errorf("ingest: replica phase %q is not agent-reportable", obs.Phase)
	}
	applied, err := i.store.RecordReplicaObservation(ctx, obs)
	if err != nil {
		return err
	}
	if !applied || !obs.Healthy {
		return nil
	}
	return i.store.RenewVolumeLease(ctx, obs.ReplicaID, i.now().Add(domain.VolumeLeaseTTL))
}

// ObserveVolumeSize records a volume's observed on-disk size (grow-only
// resize drift).
func (i *Ingest) ObserveVolumeSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error {
	return i.store.RecordVolumeObservedSize(ctx, volumeID, observedBytes)
}
