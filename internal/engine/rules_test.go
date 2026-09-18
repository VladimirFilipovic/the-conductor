package engine

// Table-driven tests for the cascade rules: the shared gates and retire path
// first (frozen, crash guard, placement, health, drain → reap), then the
// rolling (stateless) section. The recreate (stateful) section lives in
// recreate_rules_test.go. Dispatch mechanics and e2e scenarios live in
// reconciler_test.go.

import (
	"reflect"
	"testing"
	"time"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

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

// The one action frozen takes: frozen (current, failed) replicas are dead
// containers squatting on host reservations, and nothing below frozen can ever
// recreate them — destroy them. Live targets stay held, and the skip is
// dropped when there are destroys.
func TestDeploymentFrozenReapsFrozenReplicas(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	vol := uuid.New()
	dead1 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}
	dead2 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed, VolumeID: vol}
	alive := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseActive, Healthy: true}
	failedOutgoing := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}

	got := deploymentFrozen.then(replicaGroup{
		Desired:          desiredState{Slot: slot, Status: domain.DeploymentFailed},
		TargetReplicas:   []replica{alive},
		FrozenReplicas:   []replica{dead1, dead2},
		OutgoingReplicas: []replica{failedOutgoing}, // outgoing stays frozen too — old side may still serve
	})

	want := []Intent{
		{Kind: IntentDestroy, ReplicaID: dead1.ID},
		{Kind: IntentDestroy, ReplicaID: dead2.ID, VolumeID: vol}, // lease rides along for release
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deploymentFrozen.then() = %v, want %v", got, want)
	}
}

// buildReplicaGroups sorts every replica into exactly one of three buckets.
// Failed is the axis that matters here: a failed CURRENT replica is frozen (it
// holds a slot), a failed outgoing one is just outgoing (reapFailedOutgoing
// owns it), and a drained current one is outgoing regardless of phase.
func TestBuildReplicaGroupsBucketsFrozen(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	dep := uuid.New()
	now := time.Unix(1_000_000, 0)
	live := replica{ID: uuid.New(), Slot: slot, Current: true, Phase: domain.ReplicaPhaseActive}
	frozen := replica{ID: uuid.New(), Slot: slot, Current: true, Phase: domain.ReplicaPhaseFailed}
	drainedFailed := replica{ID: uuid.New(), Slot: slot, Current: true, Phase: domain.ReplicaPhaseFailed, DrainedAt: now}
	oldFailed := replica{ID: uuid.New(), Slot: slot, Current: false, Phase: domain.ReplicaPhaseFailed}
	oldLive := replica{ID: uuid.New(), Slot: slot, Current: false, Phase: domain.ReplicaPhaseActive}

	groups := buildReplicaGroups(stateSnapshot{
		observedAt: now,
		desired:    []desiredState{{Slot: slot, DeploymentID: dep}},
		replicas:   []replica{live, frozen, drainedFailed, oldFailed, oldLive},
	})
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	ids := func(rs []replica) []uuid.UUID {
		out := make([]uuid.UUID, len(rs))
		for i, r := range rs {
			out[i] = r.ID
		}
		return out
	}
	if got := ids(g.TargetReplicas); !reflect.DeepEqual(got, []uuid.UUID{live.ID}) {
		t.Errorf("targets = %v, want only the live current replica", got)
	}
	if got := ids(g.FrozenReplicas); !reflect.DeepEqual(got, []uuid.UUID{frozen.ID}) {
		t.Errorf("frozen = %v, want only the failed current undrained replica", got)
	}
	if got := ids(g.OutgoingReplicas); !reflect.DeepEqual(got, []uuid.UUID{drainedFailed.ID, oldFailed.ID, oldLive.ID}) {
		t.Errorf("outgoing = %v, want drained + superseded regardless of phase", got)
	}
}

// Frozen replicas are out of TargetReplicas, and every gate that reads only
// targets must be blind to them — otherwise the frozen restart_count (stuck
// above max, the observation guard refuses updates) would re-trip crashLooping
// each tick, notAllHealthy would hold forever and the deadline rule would fail
// the deployment.
func TestTargetOnlyGatesIgnoreFrozen(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	frozen := replica{
		ID: uuid.New(), Phase: domain.ReplicaPhaseFailed, Healthy: false,
		RestartCount: 99, CreatedAt: now.Add(-time.Hour), // no HealthChecksPassedAt: never went healthy
	}
	rg := replicaGroup{
		Desired:        desiredState{Status: domain.DeploymentActive, Replicas: 1, RestartMax: 3, ProgressDeadline: 60},
		FrozenReplicas: []replica{frozen},
		ObservedAt:     now,
	}
	for _, rl := range []rule{crashLooping, anyHostlessReplicas, newHealthOpenPastDeadline, notAllHealthy} {
		if rl.when(rg) {
			t.Errorf("%s fired on a group whose only replica is frozen", rl.name)
		}
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

// The verdict depends on where the deployment is: mid-rollout the revision is
// suspect (fail the group); once active it was proven, so only the offenders
// freeze — each one, and none of the well-behaved ones.
func TestCrashloopVerdictByDeploymentStatus(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	dep := uuid.New()
	ok := replica{ID: uuid.New(), RestartCount: 1, Healthy: true}
	bad1 := replica{ID: uuid.New(), RestartCount: 4}
	bad2 := replica{ID: uuid.New(), RestartCount: 9}
	targets := []replica{ok, bad1, bad2}

	tests := []struct {
		name   string
		status domain.DeploymentStatus
		want   []Intent
	}{
		{
			name:   "active: freeze exactly the offenders",
			status: domain.DeploymentActive,
			want: []Intent{
				{Kind: IntentFreezeReplica, Group: slot, ReplicaID: bad1.ID},
				{Kind: IntentFreezeReplica, Group: slot, ReplicaID: bad2.ID},
			},
		},
		{
			name:   "pending rollout: fail the deployment",
			status: domain.DeploymentPending,
			want:   []Intent{{Kind: IntentFail, Group: slot, DeploymentID: dep}},
		},
		{
			name:   "draining rollout: fail the deployment",
			status: domain.DeploymentDraining,
			want:   []Intent{{Kind: IntentFail, Group: slot, DeploymentID: dep}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crashLooping.then(replicaGroup{
				Desired:        desiredState{Slot: slot, DeploymentID: dep, Status: tt.status, RestartMax: 3},
				TargetReplicas: targets,
			})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("crashLooping.then() = %v, want %v", got, tt.want)
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

func TestNewHealthOpenPastDeadlineRule(t *testing.T) {
	now := time.Unix(1_000_000, 0)
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

func TestReapFailedOutgoingRule(t *testing.T) {
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
			name: "failed outgoing fires",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseFailed}}},
			want: true,
		},
		{
			name: "active outgoing holds (drain path owns it)",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseActive}}},
			want: false,
		},
		{
			name: "draining outgoing holds (reapDrained owns it)",
			in:   replicaGroup{OutgoingReplicas: []replica{{Phase: domain.ReplicaPhaseDraining}}},
			want: false,
		},
		{
			name: "failed target is ignored — deploymentFrozen sweeps current failures",
			in:   replicaGroup{TargetReplicas: []replica{{Phase: domain.ReplicaPhaseFailed}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reapFailedOutgoing.when(tt.in)
			if got != tt.want {
				t.Errorf("reapFailedOutgoing.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReapFailedOutgoingDestroysOnlyFailed(t *testing.T) {
	failed1 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}
	failed2 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}
	draining := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseDraining}

	got := reapFailedOutgoing.then(replicaGroup{
		OutgoingReplicas: []replica{failed1, draining, failed2},
	})

	want := []Intent{
		{Kind: IntentDestroy, ReplicaID: failed1.ID},
		{Kind: IntentDestroy, ReplicaID: failed2.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reapFailedOutgoing.then() = %v, want %v", got, want)
	}
}

// --- rolling cascade (stateless): surge-based ramp-up, scale-down, completion ---

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
		{
			name: "frozen replica holds its slot: no replacement",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 2},
				TargetReplicas: []replica{healthy},
				FrozenReplicas: []replica{{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}},
			},
			want: false,
		},
		{
			name: "frozen slot plus a real deficit still fires for the deficit",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 3},
				TargetReplicas: []replica{healthy},
				FrozenReplicas: []replica{{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}},
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

// A frozen slot is subtracted from the deficit but proves nothing: with a
// healthy sibling the rest comes up in a batch, with every live replica gone
// (only frozen left) the first replacement is a canary.
func TestRollingRampUpDeficitExcludesFrozenSlots(t *testing.T) {
	slot := replicaSlot{uuid.New(), "eu-west"}
	healthy := replica{ID: uuid.New(), Healthy: true}
	frozen := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}

	tests := []struct {
		name string
		in   replicaGroup
		want []Intent
	}{
		{
			name: "proven version: deficit minus the frozen slot, in one batch",
			in: replicaGroup{
				Desired:        desiredState{Slot: slot, Replicas: 4},
				TargetReplicas: []replica{healthy},
				FrozenReplicas: []replica{frozen},
			},
			want: []Intent{{Kind: IntentCreate, Group: slot}, {Kind: IntentCreate, Group: slot}},
		},
		{
			name: "only frozen left: canary",
			in: replicaGroup{
				Desired:        desiredState{Slot: slot, Replicas: 3},
				FrozenReplicas: []replica{frozen},
			},
			want: []Intent{{Kind: IntentCreate, Group: slot}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingRampUp.then(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rollingRampUp.then() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRollingScaleDownRule(t *testing.T) {
	healthy := replica{ID: uuid.New(), Healthy: true}

	tests := []struct {
		name string
		in   replicaGroup
		want bool
	}{
		{
			name: "above desired fires",
			in:   replicaGroup{Desired: desiredState{Replicas: 1}, TargetReplicas: []replica{healthy, healthy}},
			want: true,
		},
		{
			name: "at desired holds",
			in:   replicaGroup{Desired: desiredState{Replicas: 2}, TargetReplicas: []replica{healthy, healthy}},
			want: false,
		},
		{
			name: "below desired holds",
			in:   replicaGroup{Desired: desiredState{Replicas: 3}, TargetReplicas: []replica{healthy}},
			want: false,
		},
		{
			name: "outgoing replicas do not count toward the excess",
			in: replicaGroup{
				Desired:          desiredState{Replicas: 1},
				TargetReplicas:   []replica{healthy},
				OutgoingReplicas: []replica{{ID: uuid.New()}, {ID: uuid.New()}},
			},
			want: false,
		},
		{
			name: "frozen replicas count toward the excess (they hold slots)",
			in: replicaGroup{
				Desired:        desiredState{Replicas: 1},
				TargetReplicas: []replica{healthy},
				FrozenReplicas: []replica{{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingScaleDown.when(tt.in)
			if got != tt.want {
				t.Errorf("rollingScaleDown.when() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Shrinking sacrifices the least-established replicas: exactly the excess
// count is drained, newest first, regardless of input order.
func TestRollingScaleDownDrainsNewestFirst(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	oldest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-3 * time.Hour)}
	middle := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-2 * time.Hour)}
	newest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-1 * time.Hour)}

	got := rollingScaleDown.then(replicaGroup{
		Desired:        desiredState{Replicas: 1},
		TargetReplicas: []replica{middle, oldest, newest}, // deliberately unordered
	})

	want := []Intent{
		{Kind: IntentDrain, ReplicaID: newest.ID},
		{Kind: IntentDrain, ReplicaID: middle.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rollingScaleDown.then() = %v, want %v", got, want)
	}
}

// Shrinking spends the excess on frozen replicas first — destroyed outright,
// there is no traffic to drain off a dead container — and only what remains
// of the excess costs a live replica (newest first). A scale-down that fits
// entirely in the frozen set drains nobody.
func TestRollingScaleDownDestroysFrozenBeforeDrainingLive(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	vol := uuid.New()
	old := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-3 * time.Hour)}
	newest := replica{ID: uuid.New(), Healthy: true, CreatedAt: now.Add(-1 * time.Hour), Revision: 4}
	frozen1 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}
	frozen2 := replica{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed, VolumeID: vol}

	tests := []struct {
		name    string
		desired int32
		want    []Intent
	}{
		{
			name:    "excess fits in frozen: destroy only, no drain",
			desired: 3,
			want:    []Intent{{Kind: IntentDestroy, ReplicaID: frozen1.ID}},
		},
		{
			name:    "excess spans both: all frozen destroyed, then newest live drained",
			desired: 1,
			want: []Intent{
				{Kind: IntentDestroy, ReplicaID: frozen1.ID},
				{Kind: IntentDestroy, ReplicaID: frozen2.ID, VolumeID: vol},
				{Kind: IntentDrain, ReplicaID: newest.ID, Revision: 4},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rollingScaleDown.then(replicaGroup{
				Desired:        desiredState{Replicas: tt.desired},
				TargetReplicas: []replica{old, newest},
				FrozenReplicas: []replica{frozen1, frozen2},
			})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rollingScaleDown.then() = %v, want %v", got, tt.want)
			}
		})
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
		{
			name: "frozen slot counts as converged (rollback onto a degraded revision completes)",
			in: replicaGroup{
				Desired:        desiredState{Status: domain.DeploymentDraining, Replicas: 2},
				TargetReplicas: []replica{healthy},
				FrozenReplicas: []replica{{ID: uuid.New(), Phase: domain.ReplicaPhaseFailed}},
			},
			want: true,
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
