package api

// ObservedState unit tests: the observation guards that stand between agent reports
// and the store, and the clock/lease coupling.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

type fakeObservedStateStore struct {
	// dropObservations simulates the SQL guard rejecting reports as stale
	// (replica already in a terminal/orchestrator-owned phase).
	dropObservations bool

	heartbeats   []string // "host@time"
	observations []storage.ReplicaObservation
	renewals     []string // "replica until time"
	volumeSizes  []string // "volume=bytes"
}

func (f *fakeObservedStateStore) RecordVolumeObservedSize(_ context.Context, volumeID uuid.UUID, observedBytes int64) error {
	f.volumeSizes = append(f.volumeSizes, volumeID.String()+"="+strconv.FormatInt(observedBytes, 10))
	return nil
}

func (f *fakeObservedStateStore) RecordHostHeartbeat(_ context.Context, hostID uuid.UUID, observedAt time.Time) error {
	f.heartbeats = append(f.heartbeats, hostID.String()+"@"+observedAt.UTC().Format(time.RFC3339))
	return nil
}

func (f *fakeObservedStateStore) RecordReplicaObservation(_ context.Context, obs storage.ReplicaObservation) (bool, error) {
	if f.dropObservations {
		return false, nil
	}
	f.observations = append(f.observations, obs)
	return true, nil
}

func (f *fakeObservedStateStore) RenewVolumeLease(_ context.Context, replicaID uuid.UUID, expiresAt time.Time) error {
	f.renewals = append(f.renewals, replicaID.String()+" until "+expiresAt.UTC().Format(time.RFC3339))
	return nil
}

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestObserved(store ObservedStateStore) *ObservedState {
	return NewObservedState(store).WithClock(func() time.Time { return fixedNow })
}

func pinnedID(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

// The heartbeat's observation time is the ingest clock, never the agent's — a
// skewed agent clock must not keep a dead host alive.
func TestRecordHeartbeatStampsIngestClock(t *testing.T) {
	store := &fakeObservedStateStore{}
	if err := newTestObserved(store).RecordHeartbeat(context.Background(), pinnedID(1)); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}
	want := pinnedID(1).String() + "@2026-01-01T12:00:00Z"
	if len(store.heartbeats) != 1 || store.heartbeats[0] != want {
		t.Fatalf("heartbeats = %v, want [%s]", store.heartbeats, want)
	}
}

// An unknown phase is agent garbage — rejected before it can hit the CHECK
// constraint as a driver error.
func TestObserveReplicaRejectsInvalidPhase(t *testing.T) {
	store := &fakeObservedStateStore{}
	in := newTestObserved(store)

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
	store := &fakeObservedStateStore{}
	in := newTestObserved(store)
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
	store := &fakeObservedStateStore{dropObservations: true}
	if err := newTestObserved(store).ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
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
	store := &fakeObservedStateStore{}
	in := newTestObserved(store)
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

// A disk size lands as reported — including one smaller than before, which is
// how a recreated disk shows up as drift again. Zero and negative are garbage.
func TestObserveVolumeSizeRecordsPositiveOnly(t *testing.T) {
	store := &fakeObservedStateStore{}
	in := newTestObserved(store)
	ctx := context.Background()

	for _, bad := range []int64{0, -1} {
		if err := in.ObserveVolumeSize(ctx, pinnedID(1), bad); err == nil {
			t.Errorf("size %d accepted from agent", bad)
		}
	}
	if err := in.ObserveVolumeSize(ctx, pinnedID(1), 8); err != nil {
		t.Fatalf("ObserveVolumeSize: %v", err)
	}
	if err := in.ObserveVolumeSize(ctx, pinnedID(1), 4); err != nil {
		t.Fatalf("ObserveVolumeSize (shrink): %v", err)
	}
	want := []string{pinnedID(1).String() + "=8", pinnedID(1).String() + "=4"}
	if len(store.volumeSizes) != 2 || store.volumeSizes[0] != want[0] || store.volumeSizes[1] != want[1] {
		t.Fatalf("recorded = %v, want %v", store.volumeSizes, want)
	}
}
