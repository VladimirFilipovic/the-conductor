package engine

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

func TestDeploymentFrozenRule(t *testing.T) {
	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "failed deployment freezes",
			in:   replicaGroup{Desired: desiredState{Status: domain.DeploymentFailed}},
			want: true,
		},
		{
			name: "active deployment is not frozen",
			in:   replicaGroup{Desired: desiredState{Status: domain.DeploymentActive}},
			want: false,
		},
		{
			name: "draining deployment is not frozen",
			in:   replicaGroup{Desired: desiredState{Status: domain.DeploymentDraining}},
			want: false,
		},
		{
			name: "zero-status (orphan) group is not frozen",
			in:   replicaGroup{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deploymentFrozen.when(tt.in)
			if got != tt.want {
				t.Errorf("deploymentFrozen.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeploymentFrozenHolds(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	rg := replicaGroup{Desired: desiredState{Slot: slot, Status: domain.DeploymentFailed}}

	got := deploymentFrozen.then(rg)
	want := []Intent{{Kind: IntentSkip, Group: slot}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deploymentFrozen.then() = %v, want %v", got, want)
	}
}

// Frozen means frozen: whatever else is true of a failed group — a breached
// restart budget (no re-fail), a hostless replica (no re-placement), or a
// fully recovered target set (no un-freeze via rolloutComplete) — the cascade
// must stop at deploymentFrozen and emit only the skip.
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

func TestCrashloopRule(t *testing.T) {
	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "target replica over restart_max trips",
			in: replicaGroup{
				Desired:        desiredState{RestartMax: 3},
				TargetReplicas: []replica{{RestartCount: 4}},
			},
			want: true,
		},
		{
			name: "no replicas, not failed, holds",
			in:   replicaGroup{Desired: desiredState{RestartMax: 3}},
			want: false,
		},
		{
			name: "restart_count equal to max is within budget",
			in: replicaGroup{
				Desired:        desiredState{RestartMax: 3},
				TargetReplicas: []replica{{RestartCount: 3}},
			},
			want: false,
		},
		{
			name: "outgoing replica breach is ignored",
			in: replicaGroup{
				Desired:          desiredState{RestartMax: 3},
				OutgoingReplicas: []replica{{RestartCount: 9}},
			},
			want: false,
		},
		{
			name: "one breaching target among healthy ones trips",
			in: replicaGroup{
				Desired:        desiredState{RestartMax: 3},
				TargetReplicas: []replica{{RestartCount: 0}, {RestartCount: 5}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crashLooping.when(tt.in)
			if got != tt.want {
				t.Errorf("crashLooping.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAnyHostlessReplicasRule(t *testing.T) {
	placed := replica{ID: uuid.New(), HostID: uuid.New()}
	hostless := replica{ID: uuid.New()} // zero HostID == unplaced

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "no replicas holds",
			in:   replicaGroup{},
			want: false,
		},
		{
			name: "all targets placed holds",
			in:   replicaGroup{TargetReplicas: []replica{placed}},
			want: false,
		},
		{
			name: "hostless target fires",
			in:   replicaGroup{TargetReplicas: []replica{hostless}},
			want: true,
		},
		{
			name: "one hostless among placed targets fires",
			in:   replicaGroup{TargetReplicas: []replica{placed, hostless}},
			want: true,
		},
		{
			name: "hostless outgoing replica is ignored",
			in:   replicaGroup{OutgoingReplicas: []replica{hostless}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := anyHostlessReplicas.when(tt.in)
			if got != tt.want {
				t.Errorf("anyHostlessReplicas.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewHealthOpenPastDeadlineRule(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// deadline 60s; born just past it vs comfortably within it.
	born := func(ago time.Duration) time.Time { return now.Add(-ago) }

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "up past deadline, never passed → stalled",
			in: replicaGroup{
				Desired:        desiredState{ProgressDeadline: 60},
				TargetReplicas: []replica{{CreatedAt: born(61 * time.Second)}},
				ObservedAt:     now,
			},
			want: true,
		},
		{
			name: "within deadline holds",
			in: replicaGroup{
				Desired:        desiredState{ProgressDeadline: 60},
				TargetReplicas: []replica{{CreatedAt: born(59 * time.Second)}},
				ObservedAt:     now,
			},
			want: false,
		},
		{
			name: "exactly at deadline is not yet past (strictly >)",
			in: replicaGroup{
				Desired:        desiredState{ProgressDeadline: 60},
				TargetReplicas: []replica{{CreatedAt: born(60 * time.Second)}},
				ObservedAt:     now,
			},
			want: false,
		},
		{
			name: "passed health check before deadline is healthy, not stalled",
			in: replicaGroup{
				Desired: desiredState{ProgressDeadline: 60},
				TargetReplicas: []replica{{
					CreatedAt:            born(300 * time.Second),
					HealthChecksPassedAt: born(280 * time.Second),
				}},
				ObservedAt: now,
			},
			want: false,
		},
		{
			name: "outgoing stuck replica is ignored",
			in: replicaGroup{
				Desired:          desiredState{ProgressDeadline: 60},
				OutgoingReplicas: []replica{{CreatedAt: born(999 * time.Second)}},
				ObservedAt:       now,
			},
			want: false,
		},
		{
			name: "no target replicas holds",
			in: replicaGroup{
				Desired:    desiredState{ProgressDeadline: 60},
				ObservedAt: now,
			},
			want: false,
		},
		{
			name: "one stalled among a healthy target trips",
			in: replicaGroup{
				Desired: desiredState{ProgressDeadline: 60},
				TargetReplicas: []replica{
					{CreatedAt: born(300 * time.Second), HealthChecksPassedAt: born(280 * time.Second)},
					{CreatedAt: born(120 * time.Second)},
				},
				ObservedAt: now,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newHealthOpenPastDeadline.when(tt.in)
			if got != tt.want {
				t.Errorf("newHealthOpenPastDeadline.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNotAllHealthyRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}
	unhealthy := replica{ID: uuid.New()} // zero value Healthy == false

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "no target replicas does not hold (cold start falls through to ramp-up)",
			in:   replicaGroup{},
			want: false,
		},
		{
			name: "all targets healthy does not hold",
			in:   replicaGroup{TargetReplicas: []replica{healthy, healthy}},
			want: false,
		},
		{
			name: "single unhealthy target holds",
			in:   replicaGroup{TargetReplicas: []replica{unhealthy}},
			want: true,
		},
		{
			name: "one unhealthy among healthy targets holds",
			in:   replicaGroup{TargetReplicas: []replica{healthy, unhealthy}},
			want: true,
		},
		{
			name: "unhealthy outgoing replica is ignored",
			in:   replicaGroup{OutgoingReplicas: []replica{unhealthy}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := notAllHealthy.when(tt.in)
			if got != tt.want {
				t.Errorf("notAllHealthy.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The rule is a hold: it matches to break the cascade (freezing ramp-up while a
// replica boots) and emits an explicit skip for the group's slot.
func TestNotAllHealthyHolds(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	rg := replicaGroup{
		Desired:        desiredState{Slot: slot},
		TargetReplicas: []replica{{ID: uuid.New()}},
	}

	got := notAllHealthy.then(rg)
	want := []Intent{{Kind: IntentSkip, Group: slot}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("notAllHealthy.then() = %v, want %v", got, want)
	}
}

func TestRollingRampUpRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}
	unhealthy := replica{ID: uuid.New()}
	outgoing := replica{ID: uuid.New()}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "at desired holds",
			in:   replicaGroup{Desired: desiredState{Replicas: 2}, TargetReplicas: []replica{healthy, healthy}},
			want: false,
		},
		{
			name: "above desired holds",
			in:   replicaGroup{Desired: desiredState{Replicas: 1}, TargetReplicas: []replica{healthy, healthy}},
			want: false,
		},
		{
			name: "cold start below desired fires",
			in:   replicaGroup{Desired: desiredState{Replicas: 3}},
			want: true,
		},
		{
			name: "scale-up below desired fires",
			in:   replicaGroup{Desired: desiredState{Replicas: 5}, TargetReplicas: []replica{healthy, healthy}},
			want: true,
		},
		{
			name: "rolling update below desired fires",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 3},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{outgoing, outgoing},
			},
			want: true,
		},
		{
			name: "unhealthy targets do not count toward desired",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 2},
				TargetReplicas: []replica{healthy, unhealthy},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingRampUp.when(tt.in)
			if got != tt.want {
				t.Errorf("rollingRampUp.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Canary-then-batch, the batch half: one healthy target proves the version, so
// the remaining deficit is created in a single batch — with or without an old
// version still draining alongside.
func TestRollingRampUpBatchCreatesDeficit(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
	}{
		{
			name: "scale-up of a proven version",
			in: replicaGroup{
				Desired:        desiredState{Slot: slot, Replicas: 3},
				TargetReplicas: []replica{healthy}, // deficit of 2
			},
		},
		{
			name: "rolling update after the canary went healthy",
			in: replicaGroup{
				Desired:          desiredState{Slot: slot, Replicas: 3},
				TargetReplicas:   []replica{healthy}, // the canary
				OutgoingReplicas: []replica{{ID: uuid.New()}, {ID: uuid.New()}, {ID: uuid.New()}},
			},
		},
	}

	want := []Intent{
		{Kind: IntentCreate, Group: slot},
		{Kind: IntentCreate, Group: slot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingRampUp.then(tt.in)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("rollingRampUp.then() = %v, want %v", got, want)
			}
		})
	}
}

// Canary-then-batch, the canary half: an unproven revision (zero healthy
// targets) gets exactly one replica regardless of the deficit — a DOA version
// costs one zombie, not N. Cold start and rolling update behave the same.
func TestRollingRampUpCanaryCreatesOne(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}

	tests := []struct {
		name string
		in   replicaGroup
	}{
		{
			name: "cold start",
			in:   replicaGroup{Desired: desiredState{Slot: slot, Replicas: 3}},
		},
		{
			name: "rolling update with old version still up",
			in: replicaGroup{
				Desired:          desiredState{Slot: slot, Replicas: 3},
				OutgoingReplicas: []replica{{ID: uuid.New()}, {ID: uuid.New()}, {ID: uuid.New()}},
			},
		},
	}

	want := []Intent{{Kind: IntentCreate, Group: slot}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingRampUp.then(tt.in)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("rollingRampUp.then() = %v, want %v", got, want)
			}
		})
	}
}

func TestAnyHostlessReplicasEmitsAssignHost(t *testing.T) {
	placed := replica{ID: uuid.New(), HostID: uuid.New()}
	lost1 := replica{ID: uuid.New()}
	lost2 := replica{ID: uuid.New()}

	got := anyHostlessReplicas.then(replicaGroup{
		TargetReplicas:   []replica{placed, lost1, lost2},
		OutgoingReplicas: []replica{{ID: uuid.New()}}, // hostless outgoing must be ignored
	})

	want := []Intent{
		{Kind: IntentAssignHost, ReplicaID: lost1.ID},
		{Kind: IntentAssignHost, ReplicaID: lost2.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("then() = %v, want %v", got, want)
	}
}

func TestRolloutComplete(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "activates when reached desired replicas and all outgoing drained",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentDraining, Replicas: 1},
				TargetReplicas: []replica{healthy},
			},
			want: true,
		},
		{
			name: "outgoing still present does not activate",
			in: replicaGroup{
				Desired:          desiredState{Status: domain.DeploymentDraining, Replicas: 1},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{{ID: uuid.New()}},
			},
			want: false,
		},
		{
			name: "unhealthy target does not count toward desired",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentDraining, Replicas: 2},
				TargetReplicas: []replica{healthy, {ID: uuid.New()}, {ID: uuid.New(), Healthy: false}},
			},
			want: false,
		},
		{
			name: "already active does not re-emit",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentActive, Replicas: 1},
				TargetReplicas: []replica{healthy},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rolloutComplete.when(tt.in)
			if got != tt.want {
				t.Errorf("rolloutComplete.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRolloutCompleteEmitsComplete(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	rg := replicaGroup{
		Desired:        desiredState{Slot: slot, Status: domain.DeploymentDraining, Replicas: 1},
		TargetReplicas: []replica{{ID: uuid.New(), Healthy: true}},
	}

	got := rolloutComplete.then(rg)
	want := []Intent{{Kind: IntentComplete, Group: slot}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rolloutComplete.then() = %v, want %v", got, want)
	}
}

func TestDrainOutgoingRule(t *testing.T) {
	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "no outgoing holds",
			in:   replicaGroup{},
			want: false,
		},
		{
			name: "active outgoing fires",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseActive}}},
			want: true,
		},
		{
			name: "outgoing still booting fires (aborted mid-rollout)",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseStarting}}},
			want: true,
		},
		{
			name: "already draining outgoing holds",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseDraining}}},
			want: false,
		},
		{
			name: "failed (terminal) outgoing is never drained",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseFailed}}},
			want: false,
		},
		{
			name: "one retirable among draining outgoing fires",
			in: replicaGroup{OutgoingReplicas: []replica{
				{Phase: domain.ReplicaPhaseDraining},
				{Phase: domain.ReplicaPhaseHealthy},
			}},
			want: true,
		},
		{
			name: "target replicas are ignored",
			in:   replicaGroup{TargetReplicas: []replica{{Phase: domain.ReplicaPhaseActive}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := drainOutgoing.when(tt.in)
			if got != tt.want {
				t.Errorf("drainOutgoing.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// All retirable outgoing drain in one batch; already-draining and terminal
// replicas are left alone.
func TestDrainOutgoingDrainsAllRetirable(t *testing.T) {
	active := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseActive}
	booting := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseStarting}
	draining := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseDraining}
	failed := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}

	got := drainOutgoing.then(replicaGroup{
		OutgoingReplicas: []replica{active, draining, failed, booting},
	})

	want := []Intent{
		{Kind: IntentDrain, ReplicaID: active.ID},
		{Kind: IntentDrain, ReplicaID: booting.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drainOutgoing.then() = %v, want %v", got, want)
	}
}

func TestReapDrainedRule(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	drainedAgo := func(ago time.Duration) time.Time { return now.Add(-ago) }

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "window elapsed fires",
			in: replicaGroup{
				OutgoingReplicas: []replica{{DrainSeconds: 30, DrainedAt: drainedAgo(31 * time.Second)}},
				ObservedAt:       now,
			},
			want: true,
		},
		{
			name: "within window holds",
			in: replicaGroup{
				OutgoingReplicas: []replica{{DrainSeconds: 30, DrainedAt: drainedAgo(29 * time.Second)}},
				ObservedAt:       now,
			},
			want: false,
		},
		{
			name: "exactly at window boundary is not yet past (strictly >)",
			in: replicaGroup{
				OutgoingReplicas: []replica{{DrainSeconds: 30, DrainedAt: drainedAgo(30 * time.Second)}},
				ObservedAt:       now,
			},
			want: false,
		},
		{
			name: "undrained outgoing holds",
			in: replicaGroup{
				OutgoingReplicas: []replica{{DrainSeconds: 30}},
				ObservedAt:       now,
			},
			want: false,
		},
		{
			name: "drain window is per replica, not per group",
			in: replicaGroup{
				OutgoingReplicas: []replica{
					{DrainSeconds: 300, DrainedAt: drainedAgo(60 * time.Second)},
					{DrainSeconds: 30, DrainedAt: drainedAgo(60 * time.Second)},
				},
				ObservedAt: now,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reapDrained.when(tt.in)
			if got != tt.want {
				t.Errorf("reapDrained.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReapDrainedDestroysOnlyElapsed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	elapsed := replica{ID: uuid.New(), DrainSeconds: 30, DrainedAt: now.Add(-60 * time.Second)}
	inWindow := replica{ID: uuid.New(), DrainSeconds: 300, DrainedAt: now.Add(-60 * time.Second)}
	undrained := replica{ID: uuid.New(), DrainSeconds: 30}

	got := reapDrained.then(replicaGroup{
		OutgoingReplicas: []replica{inWindow, elapsed, undrained},
		ObservedAt:       now,
	})

	want := []Intent{{Kind: IntentDestroy, ReplicaID: elapsed.ID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reapDrained.then() = %v, want %v", got, want)
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

// End-to-end through Reconcile: a scaled-down group with one healthy target
// and one drained excess replica past its window must emit exactly the
// destroy — proving the drained excess neither trips the health gate nor
// deadlocks the cascade.
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
