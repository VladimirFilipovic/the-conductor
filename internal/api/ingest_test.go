package api

// Ingest unit tests: the observation guards that stand between agent reports
// and the store, and the clock/lease coupling.

import (
	"context"
	"testing"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

type fakeIngestStore struct {
	// dropObservations simulates the SQL guard rejecting reports as stale
	// (replica already in a terminal/orchestrator-owned phase).
	dropObservations bool

	heartbeats   []string // "host@time status"
	observations []storage.ReplicaObservation
	renewals     []string // "replica until time"
}

func (f *fakeIngestStore) RecordHostHeartbeat(_ context.Context, hostID uuid.UUID, observedAt time.Time, status string) error {
	f.heartbeats = append(f.heartbeats, hostID.String()+"@"+observedAt.UTC().Format(time.RFC3339)+" "+status)
	return nil
}

func (f *fakeIngestStore) RecordReplicaObservation(_ context.Context, obs storage.ReplicaObservation) (bool, error) {
	if f.dropObservations {
		return false, nil
	}
	f.observations = append(f.observations, obs)
	return true, nil
}

func (f *fakeIngestStore) RecordVolumeObservedSize(context.Context, uuid.UUID, int64) error {
	return nil
}

func (f *fakeIngestStore) RenewVolumeLease(_ context.Context, replicaID uuid.UUID, expiresAt time.Time) error {
	f.renewals = append(f.renewals, replicaID.String()+" until "+expiresAt.UTC().Format(time.RFC3339))
	return nil
}

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestIngest(store IngestStore) *Ingest {
	return NewIngest(store).WithClock(func() time.Time { return fixedNow })
}

func pinnedID(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

// The heartbeat's observation time is the ingest clock, never the agent's — a
// skewed agent clock must not keep a dead host alive.
func TestRecordHeartbeatStampsIngestClock(t *testing.T) {
	store := &fakeIngestStore{}
	if err := newTestIngest(store).RecordHeartbeat(context.Background(), pinnedID(1), "ready"); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}
	want := pinnedID(1).String() + "@2026-01-01T12:00:00Z ready"
	if len(store.heartbeats) != 1 || store.heartbeats[0] != want {
		t.Fatalf("heartbeats = %v, want [%s]", store.heartbeats, want)
	}
}

// Agent-unreportable host statuses are rejected before they can hit the
// hosts.status CHECK (unknown value) or overwrite operator-owned desired
// state (cordoned/draining).
func TestRecordHeartbeatRejectsUnreportableStatus(t *testing.T) {
	store := &fakeIngestStore{}
	in := newTestIngest(store)
	for _, status := range []string{"up", "cordoned", "draining", ""} {
		if err := in.RecordHeartbeat(context.Background(), pinnedID(1), status); err == nil {
			t.Errorf("status %q accepted from agent", status)
		}
	}
	if len(store.heartbeats) != 0 {
		t.Fatalf("unreportable heartbeat reached the store: %v", store.heartbeats)
	}
}

// An unknown phase is agent garbage — rejected before it can hit the CHECK
// constraint as a driver error.
func TestObserveReplicaRejectsInvalidPhase(t *testing.T) {
	store := &fakeIngestStore{}
	in := newTestIngest(store)

	if err := in.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "zombie"}); err == nil {
		t.Fatal("invalid phase accepted")
	}
	if len(store.observations) != 0 {
		t.Fatalf("invalid observation reached the store: %v", store.observations)
	}
	if err := in.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	if len(store.observations) != 1 {
		t.Fatal("valid observation not recorded")
	}
}

// Orchestrator-owned phases must not enter via observation: the store guard
// only protects replicas already IN them, so ingest is the line that stops an
// agent pushing a live replica INTO draining/reaped/replacing.
func TestObserveReplicaRejectsOrchestratorOwnedPhases(t *testing.T) {
	store := &fakeIngestStore{}
	in := newTestIngest(store)
	for _, phase := range []string{"pending", "scheduling", "shifting", "draining", "reaped", "replacing"} {
		if err := in.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: phase}); err == nil {
			t.Errorf("orchestrator-owned phase %q accepted from agent", phase)
		}
	}
	if len(store.observations) != 0 {
		t.Fatalf("orchestrator-owned observation reached the store: %v", store.observations)
	}
}

// A healthy report the store drops as stale must not renew the volume lease:
// otherwise a partitioned zombie agent keeps the lease alive forever and the
// failover replica can never claim the volume.
func TestObserveReplicaStaleReportDoesNotRenewLease(t *testing.T) {
	store := &fakeIngestStore{dropObservations: true}
	if err := newTestIngest(store).ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("stale observation errored instead of dropping: %v", err)
	}
	if len(store.renewals) != 0 {
		t.Fatalf("stale healthy report renewed the lease: %v", store.renewals)
	}
}

// A healthy observation renews the holder's volume lease; an unhealthy one
// must not — an ailing replica letting its lease lapse is what frees the
// volume for failover.
func TestObserveReplicaRenewsLeaseOnlyWhenHealthy(t *testing.T) {
	store := &fakeIngestStore{}
	in := newTestIngest(store)
	ctx := context.Background()

	if err := in.ObserveReplica(ctx, storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("healthy observation: %v", err)
	}
	want := pinnedID(1).String() + " until " + fixedNow.Add(domain.VolumeLeaseTTL).UTC().Format(time.RFC3339)
	if len(store.renewals) != 1 || store.renewals[0] != want {
		t.Fatalf("renewals = %v, want [%s]", store.renewals, want)
	}
	if err := in.ObserveReplica(ctx, storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: false}); err != nil {
		t.Fatalf("unhealthy observation: %v", err)
	}
	if len(store.renewals) != 1 {
		t.Fatalf("unhealthy observation renewed the lease: %v", store.renewals)
	}
}
