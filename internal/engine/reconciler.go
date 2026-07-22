package engine

import (
	"log/slog"
	"slices"
	"time"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

type IntentKind string

const (
	IntentCreate     IntentKind = "create"
	IntentDrain      IntentKind = "drain"
	IntentDestroy    IntentKind = "destroy"
	IntentFail       IntentKind = "fail"
	IntentAssignHost IntentKind = "assign_host"
	IntentSkip       IntentKind = "skip"
	IntentComplete   IntentKind = "complete"
)

type Intent struct {
	Kind      IntentKind
	Group     replicaSlot
	ReplicaID uuid.UUID
}

// replicaSlot is the reconcile grouping key; comparable, so it indexes maps
// directly
type replicaSlot struct {
	EnvironmentServiceID uuid.UUID
	Region               string
}

type replicaGroup struct {
	// Desired is zero apart from Slot when the current deployment no longer
	// declares this slot — the group then only exists to drain its replicas.
	Desired          desiredState
	TargetReplicas   []replica
	OutgoingReplicas []replica
	// ObservedAt is the snapshot's frozen "now", so time-based rules stay pure
	// functions of their input instead of reaching for a live clock.
	ObservedAt time.Time
}

type rule struct {
	// name feeds the reconcile log: which rule decided a group's tick is the
	// most useful debugging fact the engine can emit.
	name string
	when func(rg replicaGroup) bool
	then func(rg replicaGroup) []Intent
}

// Reconciler buckets the snapshot into replica groups, diffs desired vs
// observed per group, and decides what should change. Pure: no storage
// access, so strategies stay unit-testable.
type Reconciler struct {
	// Fields so tests can inject stub rules and exercise dispatch mechanics
	// (cascade selection, first-match break) in isolation.
	rolling  []rule // stateless replicas
	recreate []rule // stateful replicas
	orphan   []rule // slots the current deployment no longer declares: drain-only
}

func NewReconciler() *Reconciler {
	return &Reconciler{
		rolling:  rollingCascade,
		recreate: recreateCascade,
		orphan:   orphanCascade,
	}
}

// TODO: feed each group to the rollout orchestrator to produce intents,
// bin-pack creates onto hosts. See docs/rollout-strategy.md.
func (r *Reconciler) Reconcile(snap stateSnapshot) []Intent {
	return r.planIntents(buildReplicaGroups(snap))
}

func buildReplicaGroups(snap stateSnapshot) []replicaGroup {
	type replicaBucket struct {
		target   []replica
		outgoing []replica
	}
	replicaIndex := make(map[replicaSlot]replicaBucket, len(snap.replicas))
	for _, r := range snap.replicas {
		b := replicaIndex[r.Slot]
		// A drained current replica (scale-down excess) is retiring, not
		// converging: bucket as outgoing so it flows through drain/reap instead
		// of holding the health gate forever.
		if r.Current && r.DrainedAt.IsZero() {
			b.target = append(b.target, r)
		} else {
			b.outgoing = append(b.outgoing, r)
		}
		replicaIndex[r.Slot] = b
	}

	groups := make([]replicaGroup, 0, len(snap.desired))
	for _, d := range snap.desired {
		b := replicaIndex[d.Slot]
		delete(replicaIndex, d.Slot)
		groups = append(groups, replicaGroup{
			Desired:          d,
			TargetReplicas:   b.target,
			OutgoingReplicas: b.outgoing,
			ObservedAt:       snap.observedAt,
		})
	}

	// Leftover slots the current deployment no longer declares (a region dropped
	// between versions) still need a group so they drain instead of leaking.
	for slot, b := range replicaIndex {
		groups = append(groups, replicaGroup{
			Desired:          desiredState{Slot: slot},
			TargetReplicas:   b.target,
			OutgoingReplicas: b.outgoing,
			ObservedAt:       snap.observedAt,
		})
	}
	return groups
}

func (r *Reconciler) planIntents(groups []replicaGroup) []Intent {
	var intents []Intent
	for _, rg := range groups {
		var rules []rule
		switch {
		case rg.Desired.DeploymentID == uuid.Nil:
			rules = r.orphan
		case rg.Desired.Stateful:
			rules = r.recreate
		default:
			rules = r.rolling
		}
		for _, rl := range rules {
			if rl.when(rg) {
				out := rl.then(rg)
				intents = append(intents, out...)
				logRuleFired(rg, rl.name, out)
				break // one active rule per group per tick
			}
		}
	}
	return intents
}

// logRuleFired keeps the tick log usable: holds (skip) recur every tick while a
// group waits, so they go to Debug; anything that changes state is rare and
// goes to Info. No rule matching at all is steady state and logs nothing.
func logRuleFired(rg replicaGroup, ruleName string, out []Intent) {
	kinds := make([]string, len(out))
	hold := true
	for i, it := range out {
		kinds[i] = string(it.Kind)
		hold = hold && it.Kind == IntentSkip
	}
	logFn := slog.Info
	if hold {
		logFn = slog.Debug
	}
	logFn("reconcile -> rule fired",
		"rule", ruleName,
		"service", rg.Desired.Slot.EnvironmentServiceID,
		"region", rg.Desired.Slot.Region,
		"intents", kinds)
}

// Cascades list rules in tick-priority order; planIntents runs the first whose
// `when` matches and stops (one active rule per group per tick).

// rolling: stateless. Shared gates → create-before-retire (surge), then retire.
var rollingCascade = []rule{
	// shared gates (both strategies)
	deploymentFrozen,          // status failed → hold everything until unlock
	crashLooping,              // restart_count > restart_max → fail, freeze
	anyHostlessReplicas,       // target lost its host → re-place
	newHealthOpenPastDeadline, // health gate open too long → fail (stalled)
	notAllHealthy,             // newest not yet healthy, within deadline → hold
	// rolling-specific: ramp/shrink to count, then retire outgoing
	rollingRampUp,      // below desired → create (canary first, then batch)
	rollingScaleDown,   // above desired → drain excess target
	rolloutComplete,    // at desired & all outgoing reaped → status active
	drainOutgoing,      // outgoing still active → drain
	reapDrained,        // outgoing drain window elapsed → destroy
	reapFailedOutgoing, // outgoing crashed terminally → destroy, free the host
}

// recreate: stateful. Same gates, but retire-before-create — single-writer
// volume lease forbids surge, so all outgoing must reap before the new comes up.
var recreateCascade = []rule{
	deploymentFrozen,
	crashLooping,
	anyHostlessReplicas,
	newHealthOpenPastDeadline,
	notAllHealthy,
	drainOutgoing, // any outgoing != reaped is retired first
	reapDrained,
	reapFailedOutgoing, // dead outgoing must clear before the lease is considered free
	recreateRampUp,     // outgoing empty & below → batch create
	recreateScaleDown,  // above → drain excess target (actuator frees the lease on reap)
	recreateComplete,   // outgoing empty & at → status active
}

// orphan groups have a zero desiredState (no deployment row), so deployment
// rules like crashLooping must never see them — RestartMax=0 would trip on any
// restart. Their only job is draining leftovers.
var orphanCascade = []rule{drainOutgoing, reapDrained, reapFailedOutgoing}

// deploymentFrozen: failed is terminal — hold the whole group until a
// rollback/redeploy re-points the deployment. Sitting first, it carries the
// "not failed" precondition for every rule below. One exception: failed target
// replicas are destroyed, since a dead container only squats on its host
// reservation. With nothing to sweep it emits an explicit skip so the held
// state stays legible downstream.
var deploymentFrozen = rule{
	name: "deploymentFrozen",
	when: func(rg replicaGroup) bool {
		return rg.Desired.Status == domain.DeploymentFailed
	},
	then: func(rg replicaGroup) []Intent {
		var intents []Intent
		for _, r := range rg.TargetReplicas {
			if failedPhase(r) {
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: r.ID})
			}
		}
		if len(intents) == 0 {
			return []Intent{{Kind: IntentSkip, Group: rg.Desired.Slot}}
		}
		return intents
	},
}

// crashLooping trips the deployment to failed once any new replica exceeds its
// restart_max. deploymentFrozen ahead of it stops the re-emit on later ticks.
var crashLooping = rule{
	name: "crashLooping",
	when: func(rg replicaGroup) bool {
		for _, r := range rg.TargetReplicas {
			if r.RestartCount > rg.Desired.RestartMax {
				return true
			}
		}
		return false
	},
	then: func(rg replicaGroup) []Intent {
		return []Intent{{Kind: IntentFail, Group: rg.Desired.Slot}}
	},
}

var anyHostlessReplicas = rule{
	name: "anyHostlessReplicas",
	when: func(rg replicaGroup) bool {
		for _, rp := range rg.TargetReplicas {
			if rp.HostID == uuid.Nil {
				return true
			}
		}
		return false
	},
	then: func(rg replicaGroup) []Intent {
		intents := []Intent{}
		for _, rp := range rg.TargetReplicas {
			if rp.HostID == uuid.Nil {
				intents = append(intents, Intent{Kind: IntentAssignHost, ReplicaID: rp.ID})
			}
		}
		return intents
	},
}

// newHealthOpenPastDeadline: a target alive past progress_deadline that never
// passed its health probe → fail (stalled), else it blocks the rollout forever.
// HealthChecksPassedAt is a once-only high-water mark, so healthy-then-crashed
// is NOT stalled.
var newHealthOpenPastDeadline = rule{
	name: "newHealthOpenPastDeadline",
	when: func(rg replicaGroup) bool {
		deadline := time.Duration(rg.Desired.ProgressDeadline) * time.Second
		for _, r := range rg.TargetReplicas {
			if r.HealthChecksPassedAt.IsZero() && rg.ObservedAt.Sub(r.CreatedAt) > deadline {
				return true
			}
		}
		return false
	},
	then: func(rg replicaGroup) []Intent {
		return []Intent{{Kind: IntentFail, Group: rg.Desired.Slot}}
	},
}

// notAllHealthy gates ramp-up/scale-down/complete while any target replica is
// not yet healthy (within deadline). Explicit skip keeps the held state legible
// downstream instead of an ambiguous empty result.
var notAllHealthy = rule{
	name: "notAllHealthy",
	when: func(rg replicaGroup) bool {
		return healthyTargets(rg) < int32(len(rg.TargetReplicas))
	},
	then: func(rg replicaGroup) []Intent { return []Intent{{Kind: IntentSkip, Group: rg.Desired.Slot}} },
}

// rollingRampUp: below desired → canary-then-batch. The first replica of a
// revision carries the whole validation risk (a DOA version costs one zombie,
// not N); once any target proves healthy the rest of the deficit comes up in
// one batch.
var rollingRampUp = rule{
	name: "rollingRampUp",
	when: func(rg replicaGroup) bool {
		return healthyTargets(rg) < rg.Desired.Replicas
	},
	then: func(rg replicaGroup) []Intent {
		// notAllHealthy above guarantees zero healthy means zero targets:
		// unproven revision → canary.
		n := rg.Desired.Replicas - healthyTargets(rg)
		if healthyTargets(rg) == 0 {
			n = 1
		}
		intents := make([]Intent, n)
		for i := range intents {
			intents[i] = Intent{Kind: IntentCreate, Group: rg.Desired.Slot}
		}
		return intents
	},
}

func healthyTargets(rg replicaGroup) int32 {
	var n int32
	for _, rp := range rg.TargetReplicas {
		if rp.Healthy {
			n++
		}
	}
	return n
}

// rollingScaleDown: more target replicas than desired → drain the excess to reap.
var rollingScaleDown = rule{
	name: "rollingScaleDown",
	when: func(rg replicaGroup) bool {
		return len(rg.TargetReplicas) > int(rg.Desired.Replicas)
	},
	then: func(rg replicaGroup) []Intent {
		n := len(rg.TargetReplicas) - int(rg.Desired.Replicas)
		intents := make([]Intent, n)
		// Drain newest-first: oldest replicas have the longest healthy history,
		// so shrinking sacrifices the least-established ones.
		sortedReplicas := slices.SortedStableFunc(slices.Values(rg.TargetReplicas), func(a, b replica) int {
			return b.CreatedAt.Compare(a.CreatedAt)
		})
		for i := range intents {
			intents[i] = Intent{Kind: IntentDrain, ReplicaID: sortedReplicas[i].ID}
		}
		return intents
	},
}

// rolloutComplete: count at desired and every outgoing replica reaped — the
// rollout has fully converged → mark the deployment active.
var rolloutComplete = rule{
	name: "rolloutComplete",
	when: func(rg replicaGroup) bool {
		// Already active → converged on an earlier tick; don't re-emit complete
		// forever. (The failed case is deploymentFrozen's job.)
		if rg.Desired.Status == domain.DeploymentActive {
			return false
		}
		return healthyTargets(rg) == rg.Desired.Replicas && len(rg.OutgoingReplicas) == 0
	},
	then: func(rg replicaGroup) []Intent { return []Intent{{Kind: IntentComplete, Group: rg.Desired.Slot}} },
}

// drainOutgoing: retirable outgoing replicas drain in one batch — nothing
// routes traffic to them, and recreate's lease wait depends on the old side
// going down promptly. Shared by all cascades; terminal replicas are never
// drained.
var drainOutgoing = rule{
	name: "drainOutgoing",
	when: func(rg replicaGroup) bool {
		return slices.ContainsFunc(rg.OutgoingReplicas, drainable)
	},
	then: func(rg replicaGroup) []Intent {
		var intents []Intent
		for _, or := range rg.OutgoingReplicas {
			if drainable(or) {
				intents = append(intents, Intent{Kind: IntentDrain, ReplicaID: or.ID})
			}
		}
		return intents
	},
}

func drainable(rp replica) bool {
	return rp.Phase != domain.ReplicaPhaseDraining && !rp.Phase.Terminal()
}

// reapDrained: drain window elapsed → destroy. The single terminal transition
// for both rollout and scale-down drains. DrainSeconds is read per replica: an
// outgoing keeps its own deployment's graceful window, not the target's.
var reapDrained = rule{
	name: "reapDrained",
	when: func(rg replicaGroup) bool {
		return slices.ContainsFunc(rg.OutgoingReplicas, func(rp replica) bool {
			return drainWindowElapsed(rp, rg.ObservedAt)
		})
	},
	then: func(rg replicaGroup) []Intent {
		var intents []Intent
		for _, or := range rg.OutgoingReplicas {
			if drainWindowElapsed(or, rg.ObservedAt) {
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: or.ID})
			}
		}
		return intents
	},
}

// reapFailedOutgoing: a failed outgoing serves nothing yet holds its host
// reservation and blocks the len(outgoing)==0 convergence checks → destroy
// directly, no drain window (no traffic to bleed off a crashed container).
// Failed TARGET replicas are deploymentFrozen's job.
var reapFailedOutgoing = rule{
	name: "reapFailedOutgoing",
	when: func(rg replicaGroup) bool {
		return slices.ContainsFunc(rg.OutgoingReplicas, failedPhase)
	},
	then: func(rg replicaGroup) []Intent {
		var intents []Intent
		for _, or := range rg.OutgoingReplicas {
			if failedPhase(or) {
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: or.ID})
			}
		}
		return intents
	},
}

func failedPhase(rp replica) bool { return rp.Phase == domain.ReplicaPhaseFailed }

func drainWindowElapsed(rp replica, now time.Time) bool {
	if rp.DrainedAt.IsZero() {
		return false
	}
	return now.Sub(rp.DrainedAt) > time.Duration(rp.DrainSeconds)*time.Second
}

// recreateRampUp: lease free (all outgoing reaped) and below desired → create
// the whole deficit. No canary: retire-before-create already serialized the
// rollout, and every tick of downtime costs availability.
var recreateRampUp = rule{
	name: "recreateRampUp",
	when: func(rg replicaGroup) bool {
		return len(rg.OutgoingReplicas) == 0 && healthyTargets(rg) < rg.Desired.Replicas
	},
	then: func(rg replicaGroup) []Intent {
		n := rg.Desired.Replicas - healthyTargets(rg)
		intents := make([]Intent, n)
		for i := range intents {
			intents[i] = Intent{Kind: IntentCreate, Group: rg.Desired.Slot}
		}
		return intents
	},
}

// recreateScaleDown reuses rollingScaleDown: draining the newest excess is
// version-agnostic — the actuator frees the volume lease on reap. Distinct name
// so the reconcile log attributes the tick to the recreate cascade.
var recreateScaleDown = rule{name: "recreateScaleDown", when: rollingScaleDown.when, then: rollingScaleDown.then}

// recreateComplete reuses rolloutComplete: convergence reads the same for both
// strategies. Distinct name for reconcile log attribution.
var recreateComplete = rule{name: "recreateComplete", when: rolloutComplete.when, then: rolloutComplete.then}
