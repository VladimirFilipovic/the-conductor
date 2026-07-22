package engine

// Reconciler mechanics: snapshot → group bucketing (buildReplicaGroups),
// planIntents dispatch (stub rules), and end-to-end scenarios through
// Reconcile. Rule-level tests live in rules_*_test.go.

import (
	"reflect"
	"testing"
	"time"

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

// A scale-down-drained replica keeps Current=true but is retiring: it must
// land in OutgoingReplicas, or notAllHealthy would hold the group every tick
// and reapDrained (below it) could never destroy it — a deadlock.
func TestBuildReplicaGroupsBucketsDrainedTargetAsOutgoing(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	snap := stateSnapshot{
		desired: []desiredState{{Slot: slot, Replicas: 1}},
		replicas: []replica{
			{ID: uuid.New(), Slot: slot, Current: true, Healthy: true},
			{ID: uuid.New(), Slot: slot, Current: true, DrainedAt: time.Unix(999_000, 0)},
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

// --- planIntents dispatch mechanics ---
//
// Stub rules mint sentinel IntentKinds, isolating dispatch from domain logic:
// a failure here can only mean the loop or the cascade switch — never a rule.

// matchAll always fires, tagging its output with which cascade dispatched the group.
func matchAll(kind IntentKind) rule {
	return rule{
		name: string(kind),
		when: func(replicaGroup) bool { return true },
		then: func(rg replicaGroup) []Intent {
			return []Intent{{Kind: kind, Group: rg.Desired.Slot}}
		},
	}
}

// matchNone never fires; its then failing the test doubles as proof that
// planIntents never calls then without when.
func matchNone(t *testing.T) rule {
	t.Helper()
	return rule{
		name: "matchNone",
		when: func(replicaGroup) bool { return false },
		then: func(replicaGroup) []Intent {
			t.Fatal("then called on a rule whose when returned false")
			return nil
		},
	}
}

// mustNotRun matches but fails the test if dispatched — anything after the
// first match must be dead.
func mustNotRun(t *testing.T, name string) rule {
	t.Helper()
	return rule{
		name: name,
		when: func(replicaGroup) bool { return true },
		then: func(replicaGroup) []Intent {
			t.Fatalf("%s.then called — first-match break is broken", name)
			return nil
		},
	}
}

func TestPlanIntentsSelectsCascadeByGroupKind(t *testing.T) {
	r := &Reconciler{
		rolling:  []rule{matchAll("rolling-marker")},
		recreate: []rule{matchAll("recreate-marker")},
		orphan:   []rule{matchAll("orphan-marker")},
	}

	tests := []struct {
		name  string
		group replicaGroup
		want  IntentKind
	}{
		{
			name:  "stateless group dispatches to rolling",
			group: replicaGroup{Desired: desiredState{DeploymentID: uuid.New()}},
			want:  "rolling-marker",
		},
		{
			name:  "stateful group dispatches to recreate",
			group: replicaGroup{Desired: desiredState{DeploymentID: uuid.New(), Stateful: true}},
			want:  "recreate-marker",
		},
		{
			name:  "group without a deployment dispatches to orphan",
			group: replicaGroup{Desired: desiredState{}},
			want:  "orphan-marker",
		},
		{
			// Orphan groups carry a zero desiredState, so Stateful is meaningless
			// there — no-deployment must win over the stateful flag.
			name:  "stateful orphan still dispatches to orphan",
			group: replicaGroup{Desired: desiredState{Stateful: true}},
			want:  "orphan-marker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.planIntents([]replicaGroup{tt.group})
			if len(got) != 1 || got[0].Kind != tt.want {
				t.Errorf("planIntents() = %v, want single %q intent", got, tt.want)
			}
		})
	}
}

func TestPlanIntentsFirstMatchBreaks(t *testing.T) {
	r := &Reconciler{
		rolling: []rule{
			matchAll("first"),
			mustNotRun(t, "second"),
		},
	}

	got := r.planIntents([]replicaGroup{{Desired: desiredState{DeploymentID: uuid.New()}}})
	if len(got) != 1 || got[0].Kind != "first" {
		t.Errorf("planIntents() = %v, want single %q intent", got, "first")
	}
}

func TestPlanIntentsFallsThroughNonMatchingRules(t *testing.T) {
	r := &Reconciler{
		rolling: []rule{
			matchNone(t),
			matchAll("fallback"),
		},
	}

	got := r.planIntents([]replicaGroup{{Desired: desiredState{DeploymentID: uuid.New()}}})
	if len(got) != 1 || got[0].Kind != "fallback" {
		t.Errorf("planIntents() = %v, want single %q intent", got, "fallback")
	}
}

// No rule matching is the steady state, not an error: the group simply
// produces nothing this tick.
func TestPlanIntentsNoMatchEmitsNothing(t *testing.T) {
	r := &Reconciler{rolling: []rule{matchNone(t)}}

	got := r.planIntents([]replicaGroup{{Desired: desiredState{DeploymentID: uuid.New()}}})
	if len(got) != 0 {
		t.Errorf("planIntents() = %v, want empty", got)
	}
}

// Groups are independent: each picks its own cascade, and intents come back
// concatenated in group order — a frozen or matching group must not affect
// its neighbours.
func TestPlanIntentsDispatchesEachGroupIndependently(t *testing.T) {
	r := &Reconciler{
		rolling:  []rule{matchAll("rolling-marker")},
		recreate: []rule{matchAll("recreate-marker")},
		orphan:   []rule{matchAll("orphan-marker")},
	}
	slotA := replicaSlot{uuid.New(), "eu-west"}
	slotB := replicaSlot{uuid.New(), "eu-west"}
	slotC := replicaSlot{uuid.New(), "us-east"}
	slotD := replicaSlot{uuid.New(), "eu-west"}

	got := r.planIntents([]replicaGroup{
		{Desired: desiredState{Slot: slotA, DeploymentID: uuid.New()}},
		{Desired: desiredState{Slot: slotB, DeploymentID: uuid.New(), Stateful: true}},
		{Desired: desiredState{Slot: slotC}},
		{Desired: desiredState{Slot: slotD, DeploymentID: uuid.New()}},
	})

	want := []Intent{
		{Kind: "rolling-marker", Group: slotA},
		{Kind: "recreate-marker", Group: slotB},
		{Kind: "orphan-marker", Group: slotC},
		{Kind: "rolling-marker", Group: slotD},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planIntents() = %v, want %v", got, want)
	}
}

// Frozen means frozen: whatever else is true of a failed group — breached
// restart budget, hostless replica, recovered target set — the cascade must
// stop at deploymentFrozen and emit only the skip. (Sole exception:
// failed-phase targets, which frozen sweeps itself.)
func TestFailedGroupOnlySkipsThroughCascade(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	deploymentID := uuid.New()

	tests := []struct {
		name    string
		targets []replica
	}{
		{
			name:    "breached restart budget does not re-fail",
			targets: []replica{{ID: uuid.New(), HostID: uuid.New(), RestartCount: 9}},
		},
		{
			name:    "hostless replica is not re-placed",
			targets: []replica{{ID: uuid.New()}},
		},
		{
			name:    "recovered healthy target set does not un-freeze to active",
			targets: []replica{{ID: uuid.New(), HostID: uuid.New(), Healthy: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewReconciler().planIntents([]replicaGroup{{
				Desired: desiredState{
					Slot:         slot,
					DeploymentID: deploymentID,
					Status:       domain.DeploymentFailed,
					Replicas:     1,
					RestartMax:   3,
				},
				TargetReplicas: tt.targets,
			}})
			want := []Intent{{Kind: IntentSkip, Group: slot}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("planIntents() = %v, want %v", got, want)
			}
		})
	}
}

// End-to-end through Reconcile: one healthy target plus one drained excess past
// its window must emit exactly the destroy — the drained excess neither trips
// the health gate nor deadlocks the cascade.
func TestScaleDownExcessReapsThroughCascade(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	slot := replicaSlot{uuid.New(), "eu-west"}
	excess := replica{
		ID:           uuid.New(),
		Slot:         slot,
		Current:      true,
		Phase:        domain.ReplicaPhaseDraining,
		DrainSeconds: 30,
		DrainedAt:    now.Add(-60 * time.Second),
	}
	snap := stateSnapshot{
		observedAt: now,
		desired: []desiredState{{
			Slot:             slot,
			DeploymentID:     uuid.New(),
			Status:           domain.DeploymentActive,
			Replicas:         1,
			RestartMax:       3,
			ProgressDeadline: 60,
		}},
		replicas: []replica{
			{
				ID: uuid.New(), Slot: slot, Current: true, Healthy: true,
				HostID: uuid.New(), Phase: domain.ReplicaPhaseActive,
				CreatedAt: now.Add(-2 * time.Hour), HealthChecksPassedAt: now.Add(-time.Hour),
			},
			excess,
		},
	}

	got := NewReconciler().Reconcile(snap)
	want := []Intent{{Kind: IntentDestroy, ReplicaID: excess.ID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reconcile() = %v, want %v", got, want)
	}
}
