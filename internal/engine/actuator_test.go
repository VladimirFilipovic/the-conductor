package engine

// Actuator tests: which tx calls each intent kind maps to, tx grouping (one tx
// per intent, one tx per traffic-switch batch), and the ErrConflict = non-error
// contract. The store is a recording stub — call traces are formatted strings
// so a failure prints exactly what the actuator did.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// recordingStore implements ActuatorStore: every WithReconcileTx invocation
// records its call trace, and only traces whose callback returned nil count as
// committed (the rest rolled back).
type recordingStore struct {
	committed  [][]string
	rolledBack [][]string
	// failOn injects an error on the first call whose trace starts with the
	// given prefix.
	failOn     string
	failWith   error
}

func (s *recordingStore) WithReconcileTx(_ context.Context, fn func(storage.ReconcileTx) error) error {
	tx := &recordingTx{store: s}
	if err := fn(tx); err != nil {
		s.rolledBack = append(s.rolledBack, tx.calls)
		return err
	}
	s.committed = append(s.committed, tx.calls)
	return nil
}

type recordingTx struct {
	store *recordingStore
	calls []string
}

func (t *recordingTx) record(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	t.calls = append(t.calls, call)
	if t.store.failOn != "" && len(call) >= len(t.store.failOn) && call[:len(t.store.failOn)] == t.store.failOn {
		return t.store.failWith
	}
	return nil
}

func (t *recordingTx) ActiveVolumeLease(_ context.Context, volumeID uuid.UUID) (db.VolumeLease, error) {
	return db.VolumeLease{}, t.record("ActiveVolumeLease %s", volumeID)
}

func (t *recordingTx) CreateReplica(_ context.Context, spec storage.ReplicaSpec) (db.Replica, error) {
	return db.Replica{}, t.record("CreateReplica dep=%s region=%s cpu=%d mem=%d volume=%s",
		spec.DeploymentID, spec.Region, spec.CPUMillicores, spec.MemBytes, spec.VolumeID)
}

func (t *recordingTx) AssignReplicaHost(_ context.Context, replicaID, hostID uuid.UUID) error {
	return t.record("AssignReplicaHost %s -> %s", replicaID, hostID)
}

func (t *recordingTx) AssignVolumeHost(_ context.Context, volumeID, hostID uuid.UUID) error {
	return t.record("AssignVolumeHost %s -> %s", volumeID, hostID)
}

func (t *recordingTx) AcquireVolumeLease(_ context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error {
	return t.record("AcquireVolumeLease %s by %s until %s", volumeID, replicaID, expiresAt.UTC().Format(time.RFC3339))
}

func (t *recordingTx) SetReplicaDesiredStatus(_ context.Context, replicaID uuid.UUID, status domain.ReplicaDesiredStatus) error {
	return t.record("SetReplicaDesiredStatus %s %s", replicaID, status)
}

func (t *recordingTx) SetReplicaPhase(_ context.Context, replicaID uuid.UUID, phase domain.ReplicaPhase, expectRevision int64) error {
	return t.record("SetReplicaPhase %s %s rev=%d", replicaID, phase, expectRevision)
}

func (t *recordingTx) ReleaseVolumeLease(_ context.Context, volumeID uuid.UUID) error {
	return t.record("ReleaseVolumeLease %s", volumeID)
}

func (t *recordingTx) DeleteReplica(_ context.Context, replicaID uuid.UUID) error {
	return t.record("DeleteReplica %s", replicaID)
}

func (t *recordingTx) SetDeploymentStatus(_ context.Context, deploymentID uuid.UUID, status domain.DeploymentStatus) error {
	return t.record("SetDeploymentStatus %s %s", deploymentID, status)
}

func (t *recordingTx) SetServedRevision(_ context.Context, environmentServiceID uuid.UUID, region string, deploymentID uuid.UUID) error {
	return t.record("SetServedRevision %s/%s -> %s", environmentServiceID, region, deploymentID)
}

var _ storage.ReconcileTx = (*recordingTx)(nil)

// fixedNow pins the actuator clock so lease-expiry traces are exact.
var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestActuator(store *recordingStore) *Actuator {
	return &Actuator{store: store, now: func() time.Time { return fixedNow }}
}

func assertCommitted(t *testing.T, store *recordingStore, want [][]string) {
	t.Helper()
	if len(store.committed) != len(want) {
		t.Fatalf("committed %d txs, want %d: %v", len(store.committed), len(want), store.committed)
	}
	for i := range want {
		got := store.committed[i]
		if len(got) != len(want[i]) {
			t.Fatalf("tx %d = %v, want %v", i, got, want[i])
		}
		for j := range want[i] {
			if got[j] != want[i][j] {
				t.Fatalf("tx %d call %d = %q, want %q", i, j, got[j], want[i][j])
			}
		}
	}
}

func TestApplyIntentTxMapping(t *testing.T) {
	slot := replicaSlot{pinnedID(1), "eu-west-1"}
	dep := pinnedID(2)
	rep := pinnedID(3)
	hostID := pinnedID(4)
	vol := pinnedID(5)
	leaseUntil := fixedNow.Add(volumeLeaseTTL).Format(time.RFC3339)

	tests := []struct {
		name   string
		intent Intent
		want   []string
	}{
		{
			name:   "create mints a hostless row from the spec",
			intent: Intent{Kind: IntentCreate, Group: slot, DeploymentID: dep, CPUMillicores: 500, MemBytes: 1024},
			want: []string{
				fmt.Sprintf("CreateReplica dep=%s region=eu-west-1 cpu=500 mem=1024 volume=%s", dep, uuid.Nil),
			},
		},
		{
			name:   "stateful create binds the volume",
			intent: Intent{Kind: IntentCreate, Group: slot, DeploymentID: dep, CPUMillicores: 500, MemBytes: 1024, VolumeID: vol},
			want: []string{
				fmt.Sprintf("CreateReplica dep=%s region=eu-west-1 cpu=500 mem=1024 volume=%s", dep, vol),
			},
		},
		{
			name:   "assign_host reserves the host",
			intent: Intent{Kind: IntentAssignHost, ReplicaID: rep, HostID: hostID},
			want:   []string{fmt.Sprintf("AssignReplicaHost %s -> %s", rep, hostID)},
		},
		{
			name:   "stateful assign_host acquires the lease in the same tx",
			intent: Intent{Kind: IntentAssignHost, ReplicaID: rep, HostID: hostID, VolumeID: vol},
			want: []string{
				fmt.Sprintf("AssignReplicaHost %s -> %s", rep, hostID),
				fmt.Sprintf("AcquireVolumeLease %s by %s until %s", vol, rep, leaseUntil),
			},
		},
		{
			name:   "place_volume binds the volume host",
			intent: Intent{Kind: IntentPlaceVolume, VolumeID: vol, HostID: hostID},
			want:   []string{fmt.Sprintf("AssignVolumeHost %s -> %s", vol, hostID)},
		},
		{
			name:   "plain drain is a CAS phase write, no pointer move",
			intent: Intent{Kind: IntentDrain, ReplicaID: rep, Revision: 7},
			want:   []string{fmt.Sprintf("SetReplicaPhase %s draining rev=7", rep)},
		},
		{
			name:   "destroy reclaims the row",
			intent: Intent{Kind: IntentDestroy, ReplicaID: rep},
			want:   []string{fmt.Sprintf("DeleteReplica %s", rep)},
		},
		{
			name:   "stateful destroy releases the lease first",
			intent: Intent{Kind: IntentDestroy, ReplicaID: rep, VolumeID: vol},
			want: []string{
				fmt.Sprintf("ReleaseVolumeLease %s", vol),
				fmt.Sprintf("DeleteReplica %s", rep),
			},
		},
		{
			name:   "fail freezes the deployment",
			intent: Intent{Kind: IntentFail, Group: slot, DeploymentID: dep},
			want:   []string{fmt.Sprintf("SetDeploymentStatus %s failed", dep)},
		},
		{
			name:   "complete activates and asserts the served revision",
			intent: Intent{Kind: IntentComplete, Group: slot, DeploymentID: dep},
			want: []string{
				fmt.Sprintf("SetDeploymentStatus %s active", dep),
				fmt.Sprintf("SetServedRevision %s/eu-west-1 -> %s", slot.EnvironmentServiceID, dep),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &recordingStore{}
			if err := newTestActuator(store).Apply(context.Background(), []Intent{tt.intent}); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			assertCommitted(t, store, [][]string{tt.want})
		})
	}
}

// A skip is a hold, not work: no tx at all.
func TestApplySkipOpensNoTx(t *testing.T) {
	store := &recordingStore{}
	err := newTestActuator(store).Apply(context.Background(), []Intent{{Kind: IntentSkip}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(store.committed)+len(store.rolledBack) != 0 {
		t.Fatalf("skip opened a tx: %v %v", store.committed, store.rolledBack)
	}
}

// The traffic-switch drain batch commits as ONE tx per slot: every drain plus
// the served-revision flip — while unrelated intents keep their own tx.
func TestApplySwitchBatchIsOneTx(t *testing.T) {
	slot := replicaSlot{pinnedID(1), "eu-west-1"}
	newDep := pinnedID(2)
	out1, out2 := pinnedID(3), pinnedID(4)

	store := &recordingStore{}
	err := newTestActuator(store).Apply(context.Background(), []Intent{
		{Kind: IntentDrain, Group: slot, ReplicaID: out1, Revision: 1, DeploymentID: newDep, SwitchTraffic: true},
		{Kind: IntentDrain, Group: slot, ReplicaID: out2, Revision: 2, DeploymentID: newDep, SwitchTraffic: true},
		{Kind: IntentDestroy, ReplicaID: pinnedID(9)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertCommitted(t, store, [][]string{
		{fmt.Sprintf("DeleteReplica %s", pinnedID(9))},
		{
			fmt.Sprintf("SetReplicaPhase %s draining rev=1", out1),
			fmt.Sprintf("SetReplicaPhase %s draining rev=2", out2),
			fmt.Sprintf("SetServedRevision %s/eu-west-1 -> %s", slot.EnvironmentServiceID, newDep),
		},
	})
}

// A lost CAS inside the switch batch rolls the WHOLE switch back — the pointer
// must never move off replicas that didn't actually start draining — and Apply
// still succeeds (next tick recomputes).
func TestApplySwitchBatchConflictRollsBackPointer(t *testing.T) {
	slot := replicaSlot{pinnedID(1), "eu-west-1"}
	out1, out2 := pinnedID(3), pinnedID(4)

	store := &recordingStore{
		failOn:   fmt.Sprintf("SetReplicaPhase %s", out2),
		failWith: storage.ErrConflict,
	}
	err := newTestActuator(store).Apply(context.Background(), []Intent{
		{Kind: IntentDrain, Group: slot, ReplicaID: out1, Revision: 1, DeploymentID: pinnedID(2), SwitchTraffic: true},
		{Kind: IntentDrain, Group: slot, ReplicaID: out2, Revision: 2, DeploymentID: pinnedID(2), SwitchTraffic: true},
	})
	if err != nil {
		t.Fatalf("Apply must swallow the conflict, got %v", err)
	}
	if len(store.committed) != 0 {
		t.Fatalf("conflicted batch committed: %v", store.committed)
	}
	if len(store.rolledBack) != 1 {
		t.Fatalf("rolled back %d txs, want 1", len(store.rolledBack))
	}
}

// ErrConflict on one intent is a non-event: dropped, and the remaining intents
// still apply.
func TestApplyConflictDropsIntentKeepsRest(t *testing.T) {
	conflicted, healthy := pinnedID(3), pinnedID(4)
	store := &recordingStore{
		failOn:   fmt.Sprintf("SetReplicaPhase %s", conflicted),
		failWith: storage.ErrConflict,
	}
	err := newTestActuator(store).Apply(context.Background(), []Intent{
		{Kind: IntentDrain, ReplicaID: conflicted, Revision: 1},
		{Kind: IntentDrain, ReplicaID: healthy, Revision: 1},
	})
	if err != nil {
		t.Fatalf("Apply must swallow the conflict, got %v", err)
	}
	assertCommitted(t, store, [][]string{
		{fmt.Sprintf("SetReplicaPhase %s draining rev=1", healthy)},
	})
}

// A real storage failure is not a conflict: it must bubble to the engine's
// failure counter, not be silently dropped.
func TestApplyHardErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	store := &recordingStore{failOn: "DeleteReplica", failWith: boom}
	err := newTestActuator(store).Apply(context.Background(), []Intent{
		{Kind: IntentDestroy, ReplicaID: pinnedID(3)},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Apply error = %v, want wrapped %v", err, boom)
	}
}
