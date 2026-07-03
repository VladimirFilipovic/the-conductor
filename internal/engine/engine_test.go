package engine

import (
	"testing"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

func TestBuildReplicaGroupsSplitsCurrentFromOutgoing(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	snap := stateSnapshot{
		desired: []desiredState{{Slot: slot, Replicas: 2}},
		replicas: []replica{
			{ID: uuid.New(), Slot: slot, Phase: domain.ReplicaPhaseActive, Current: true},
			{ID: uuid.New(), Slot: slot, Phase: domain.ReplicaPhaseDraining, Current: false},
		},
	}

	groups := buildReplicaGroups(snap)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if len(g.TargetReplicas) != 1 || len(g.OutgoingReplicas) != 1 {
		t.Fatalf("target/outgoing = %d/%d, want 1/1", len(g.TargetReplicas), len(g.OutgoingReplicas))
	}
}

// A slot the current deployment no longer declares (region dropped between
// versions) must still surface as a group with a zero target, or its replicas
// leak with nothing to drain them.
func TestBuildReplicaGroupsEmitsOrphanSlots(t *testing.T) {
	kept := replicaSlot{uuid.New(), "eu-west"}
	dropped := replicaSlot{kept.EnvironmentServiceID, "us-east"}
	snap := stateSnapshot{
		desired: []desiredState{{Slot: kept, Replicas: 1}},
		replicas: []replica{
			{ID: uuid.New(), Slot: kept, Current: true},
			{ID: uuid.New(), Slot: dropped, Current: false},
		},
	}

	groups := buildReplicaGroups(snap)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	var orphan *replicaGroup
	for i := range groups {
		if groups[i].Desired.Slot == dropped {
			orphan = &groups[i]
		}
	}
	if orphan == nil {
		t.Fatal("dropped slot produced no group")
	}
	if orphan.Desired.Replicas != 0 {
		t.Fatalf("orphan desired replicas = %d, want 0", orphan.Desired.Replicas)
	}
	if len(orphan.OutgoingReplicas) != 1 {
		t.Fatalf("orphan outgoing = %d, want 1", len(orphan.OutgoingReplicas))
	}
}
