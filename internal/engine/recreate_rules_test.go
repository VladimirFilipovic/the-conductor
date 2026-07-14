package engine

// Table-driven tests for the recreate (stateful) cascade rules. The shared gates
// and retire path, plus the rolling (stateless) section, live in rules_test.go;
// dispatch mechanics and e2e scenarios in reconciler_test.go.

import (
	"reflect"
	"testing"
	"time"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

// --- recreate cascade (stateful) ---
//
// The single-writer volume lease forbids surge, so recreateRampUp may only
// create once the outgoing side is empty — every outgoing replica, failed ones
// included, blocks the gate. Progress is guaranteed by reapFailedOutgoing
// destroying dead outgoing (drainOutgoing skips terminal and reapDrained needs
// DrainedAt, so without it a failed outgoing would deadlock the group).

func TestRecreateRampUpRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "cold start, no outgoing, fires",
			in:   replicaGroup{Desired: desiredState{Replicas: 2, Stateful: true}},
			want: true,
		},
		{
			name: "below desired with no outgoing fires",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 3, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: true,
		},
		{
			name: "active outgoing blocks (lease may be held)",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 2, Stateful: true},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseActive}},
			},
			want: false,
		},
		{
			name: "draining outgoing blocks until reaped",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 2, Stateful: true},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseDraining}},
			},
			want: false,
		},
		{
			name: "failed outgoing blocks until reapFailedOutgoing clears it",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 2, Stateful: true},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseFailed}},
			},
			want: false,
		},
		{
			name: "at desired holds",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 1, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: false,
		},
		{
			name: "at desired with outgoing present holds",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 1, Stateful: true},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseDraining}},
			},
			want: false,
		},
		{
			name: "above desired with no outgoing holds",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 1, Stateful: true},
				TargetReplicas: []replica{healthy, healthy},
			},
			want: false,
		},
		{
			name: "unhealthy targets do not count toward desired",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 2, Stateful: true},
				TargetReplicas: []replica{healthy, {ID: uuid.New()}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recreateRampUp.when(tt.in)
			if got != tt.want {
				t.Errorf("recreateRampUp.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Retire-before-create already serialized the rollout, so there is no canary
// half: every tick of downtime costs availability, and the whole deficit comes
// up in one batch.
func TestRecreateRampUpBatchCreatesWholeDeficit(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want []Intent
	}{
		{
			name: "cold start creates all",
			in:   replicaGroup{Desired: desiredState{Slot: slot, Replicas: 2, Stateful: true}},
			want: []Intent{
				{Kind: IntentCreate, Group: slot},
				{Kind: IntentCreate, Group: slot},
			},
		},
		{
			name: "partial fleet creates only the deficit",
			in: replicaGroup{
				Desired:        desiredState{Slot: slot, Replicas: 2, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: []Intent{{Kind: IntentCreate, Group: slot}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recreateRampUp.then(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("recreateRampUp.then() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecreateScaleDownRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "above desired fires",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 1, Stateful: true},
				TargetReplicas: []replica{healthy, healthy},
			},
			want: true,
		},
		{
			name: "at desired holds",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 2, Stateful: true},
				TargetReplicas: []replica{healthy, healthy},
			},
			want: false,
		},
		{
			name: "below desired holds",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 2, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recreateScaleDown.when(tt.in)
			if got != tt.want {
				t.Errorf("recreateScaleDown.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Same newest-first policy as rolling: the oldest replica keeps its volume
// binding and history. The lease itself is released by the actuator when the
// drained replica reaps — the reconciler only emits the drain.
func TestRecreateScaleDownDrainsNewestFirst(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	oldest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-3 * time.Hour)}
	newest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-1 * time.Hour)}

	got := recreateScaleDown.then(replicaGroup{
		Desired:        desiredState{Replicas: 1, Stateful: true},
		TargetReplicas: []replica{oldest, newest},
	})

	want := []Intent{{Kind: IntentDrain, ReplicaID: newest.ID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recreateScaleDown.then() = %v, want %v", got, want)
	}
}

// Excess of two out of three drains the two newest, newest-first, regardless of
// input order — the oldest replica keeps its volume binding and history.
func TestRecreateScaleDownDrainsTwoNewest(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	oldest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-3 * time.Hour)}
	middle := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-2 * time.Hour)}
	newest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-1 * time.Hour)}

	got := recreateScaleDown.then(replicaGroup{
		Desired:        desiredState{Replicas: 1, Stateful: true},
		TargetReplicas: []replica{middle, newest, oldest},
	})

	want := []Intent{
		{Kind: IntentDrain, ReplicaID: newest.ID},
		{Kind: IntentDrain, ReplicaID: middle.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recreateScaleDown.then() = %v, want %v", got, want)
	}
}

func TestRecreateCompleteRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "at desired with no outgoing activates",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentDraining, Replicas: 1, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: true,
		},
		{
			name: "zero desired replicas with nothing running activates",
			in: replicaGroup{
				Desired: desiredState{Status: domain.DeploymentDraining, Replicas: 0, Stateful: true},
			},
			want: true,
		},
		{
			name: "outgoing still present does not activate",
			in: replicaGroup{
				Desired:          desiredState{Status: domain.DeploymentDraining, Replicas: 1, Stateful: true},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseDraining}},
			},
			want: false,
		},
		{
			name: "failed outgoing blocks activation until reaped",
			in: replicaGroup{
				Desired:          desiredState{Status: domain.DeploymentDraining, Replicas: 1, Stateful: true},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseFailed}},
			},
			want: false,
		},
		{
			name: "unhealthy target does not count toward desired",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentDraining, Replicas: 2, Stateful: true},
				TargetReplicas: []replica{healthy, {ID: uuid.New()}},
			},
			want: false,
		},
		{
			name: "already active does not re-emit",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentActive, Replicas: 1, Stateful: true},
				TargetReplicas: []replica{healthy},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recreateComplete.when(tt.in)
			if got != tt.want {
				t.Errorf("recreateComplete.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecreateCompleteEmitsComplete(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	rg := replicaGroup{
		Desired:        desiredState{Slot: slot, Status: domain.DeploymentDraining, Replicas: 1, Stateful: true},
		TargetReplicas: []replica{{ID: uuid.New(), Healthy: true}},
	}

	got := recreateComplete.then(rg)
	want := []Intent{{Kind: IntentComplete, Group: slot}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recreateComplete.then() = %v, want %v", got, want)
	}
}
