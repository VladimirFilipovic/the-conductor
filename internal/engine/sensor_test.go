package engine

// Sensor unit tests: the staleness cutoff arithmetic, the sweep's mark-down
// fan-out, and the ingestion guards. The stale-filter SQL itself is covered by
// the e2e loop test (e2e_test.go) through the in-memory store.

import (
	"context"
	"testing"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type fakeSensorStore struct {
	dead []db.Host
	// dropObservations simulates the SQL guard rejecting reports as stale
	// (replica already in a terminal/orchestrator-owned phase).
	dropObservations bool

	heartbeats      []string // "host@time status"
	notReadyCutoffs []time.Time
	deadCutoffs     []time.Time
	downed          []uuid.UUID
	observations    []storage.ReplicaObservation
	renewals        []string // "replica until time"
}

func (f *fakeSensorStore) RecordHostHeartbeat(_ context.Context, hostID uuid.UUID, observedAt time.Time, status string) error {
	f.heartbeats = append(f.heartbeats, hostID.String()+"@"+observedAt.UTC().Format(time.RFC3339)+" "+status)
	return nil
}

func (f *fakeSensorStore) MarkStaleHostsNotReady(_ context.Context, before time.Time) (int64, error) {
	f.notReadyCutoffs = append(f.notReadyCutoffs, before)
	return 0, nil
}

func (f *fakeSensorStore) ListDeadHosts(_ context.Context, before time.Time) ([]db.Host, error) {
	f.deadCutoffs = append(f.deadCutoffs, before)
	return f.dead, nil
}

func (f *fakeSensorStore) MarkHostDown(_ context.Context, hostID uuid.UUID, _ time.Time) error {
	f.downed = append(f.downed, hostID)
	return nil
}

func (f *fakeSensorStore) RecordReplicaObservation(_ context.Context, obs storage.ReplicaObservation) (bool, error) {
	if f.dropObservations {
		return false, nil
	}
	f.observations = append(f.observations, obs)
	return true, nil
}

func (f *fakeSensorStore) ListReplicasByHost(context.Context, uuid.UUID) ([]db.Replica, error) {
	return nil, nil
}

func (f *fakeSensorStore) RecordVolumeObservedSize(context.Context, uuid.UUID, int64) error {
	return nil
}

func (f *fakeSensorStore) RenewVolumeLease(_ context.Context, replicaID uuid.UUID, expiresAt time.Time) error {
	f.renewals = append(f.renewals, replicaID.String()+" until "+expiresAt.UTC().Format(time.RFC3339))
	return nil
}

func newTestSensor(store SensorStore, now time.Time) *Sensor {
	return &Sensor{store: store, now: func() time.Time { return now }}
}

// The sweep demotes briefly-silent hosts at the notready cutoff and marks
// every dead-listed host down at the (longer) death cutoff.
func TestSweepMarksStaleHostsDown(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeSensorStore{dead: []db.Host{{ID: pinnedID(1)}, {ID: pinnedID(2)}}}

	if err := newTestSensor(store, now).sweepStaleHosts(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	wantNotReady := now.Add(-hostNotReadyAfter)
	if len(store.notReadyCutoffs) != 1 || !store.notReadyCutoffs[0].Equal(wantNotReady) {
		t.Fatalf("notready cutoff = %v, want %v", store.notReadyCutoffs, wantNotReady)
	}
	wantDead := now.Add(-hostDeadAfter)
	if len(store.deadCutoffs) != 1 || !store.deadCutoffs[0].Equal(wantDead) {
		t.Fatalf("dead cutoff = %v, want %v", store.deadCutoffs, wantDead)
	}
	if len(store.downed) != 2 || store.downed[0] != pinnedID(1) || store.downed[1] != pinnedID(2) {
		t.Fatalf("downed = %v, want both dead hosts", store.downed)
	}
}

// While uptime is shorter than hostDeadAfter the sweep must not render death
// verdicts — after a control-plane outage every heartbeat is stale by the
// plane's own absence, and agents get a full window to reconnect. Demotion to
// notready still runs (reversible, harmless).
func TestSweepStartupGraceSkipsDeathPass(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeSensorStore{dead: []db.Host{{ID: pinnedID(1)}}}
	s := newTestSensor(store, now)
	s.started = now.Add(-hostDeadAfter / 2)

	if err := s.sweepStaleHosts(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(store.notReadyCutoffs) != 1 {
		t.Fatal("notready demotion skipped during startup grace")
	}
	if len(store.deadCutoffs) != 0 || len(store.downed) != 0 {
		t.Fatalf("death pass ran during startup grace: cutoffs=%v downed=%v", store.deadCutoffs, store.downed)
	}

	s.started = now.Add(-hostDeadAfter)
	if err := s.sweepStaleHosts(context.Background()); err != nil {
		t.Fatalf("sweep after grace: %v", err)
	}
	if len(store.downed) != 1 {
		t.Fatal("death pass still skipped after the grace window elapsed")
	}
}

// The heartbeat's observation time is the sensor's clock, never the agent's —
// a skewed agent clock must not keep a dead host alive.
func TestRecordHeartbeatStampsSensorClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeSensorStore{}

	if err := newTestSensor(store, now).RecordHeartbeat(context.Background(), pinnedID(1), "ready"); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}
	want := pinnedID(1).String() + "@2026-01-01T12:00:00Z ready"
	if len(store.heartbeats) != 1 || store.heartbeats[0] != want {
		t.Fatalf("heartbeats = %v, want [%s]", store.heartbeats, want)
	}
}

// An unknown phase is agent garbage — rejected before it can hit the CHECK
// constraint as a driver error.
func TestObserveReplicaRejectsInvalidPhase(t *testing.T) {
	store := &fakeSensorStore{}
	s := newTestSensor(store, time.Now())

	err := s.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "zombie"})
	if err == nil {
		t.Fatal("invalid phase accepted")
	}
	if len(store.observations) != 0 {
		t.Fatalf("invalid observation reached the store: %v", store.observations)
	}

	if err := s.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	if len(store.observations) != 1 {
		t.Fatalf("valid observation not recorded")
	}
}

// Orchestrator-owned phases must not enter via observation: the store guard
// only protects replicas already IN them, so the sensor is the line that
// stops an agent pushing a live replica INTO draining/reaped/replacing.
func TestObserveReplicaRejectsOrchestratorOwnedPhases(t *testing.T) {
	store := &fakeSensorStore{}
	s := newTestSensor(store, time.Now())

	for _, phase := range []string{"pending", "scheduling", "shifting", "draining", "reaped", "replacing"} {
		err := s.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: phase})
		if err == nil {
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
	store := &fakeSensorStore{dropObservations: true}
	s := newTestSensor(store, time.Now())

	if err := s.ObserveReplica(context.Background(), storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("stale observation errored instead of dropping: %v", err)
	}
	if len(store.renewals) != 0 {
		t.Fatalf("stale healthy report renewed the lease: %v", store.renewals)
	}
}

// Agent-unreportable host statuses are rejected before they can hit the
// hosts.status CHECK (unknown value) or overwrite operator-owned desired
// state (cordoned/draining).
func TestRecordHeartbeatRejectsUnreportableStatus(t *testing.T) {
	store := &fakeSensorStore{}
	s := newTestSensor(store, time.Now())

	for _, status := range []string{"up", "cordoned", "draining", ""} {
		if err := s.RecordHeartbeat(context.Background(), pinnedID(1), status); err == nil {
			t.Errorf("status %q accepted from agent", status)
		}
	}
	if len(store.heartbeats) != 0 {
		t.Fatalf("unreportable heartbeat reached the store: %v", store.heartbeats)
	}
}

// A healthy observation renews the holder's volume lease; an unhealthy one
// must not — an ailing replica letting its lease lapse is what frees the
// volume for failover.
func TestObserveReplicaRenewsLeaseOnlyWhenHealthy(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeSensorStore{}
	s := newTestSensor(store, now)
	ctx := context.Background()

	if err := s.ObserveReplica(ctx, storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: true}); err != nil {
		t.Fatalf("healthy observation: %v", err)
	}
	want := pinnedID(1).String() + " until " + now.Add(volumeLeaseTTL).UTC().Format(time.RFC3339)
	if len(store.renewals) != 1 || store.renewals[0] != want {
		t.Fatalf("renewals = %v, want [%s]", store.renewals, want)
	}

	if err := s.ObserveReplica(ctx, storage.ReplicaObservation{ReplicaID: pinnedID(1), Phase: "active", Healthy: false}); err != nil {
		t.Fatalf("unhealthy observation: %v", err)
	}
	if len(store.renewals) != 1 {
		t.Fatalf("unhealthy observation renewed the lease: %v", store.renewals)
	}
}
