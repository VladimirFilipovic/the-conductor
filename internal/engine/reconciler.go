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

type replicaSlot struct {
	EnvironmentServiceID uuid.UUID
	Region               string
}

type replicaGroup struct {
	Desired          desiredState
	TargetReplicas   []replica
	OutgoingReplicas []replica
	// ObservedAt is the snapshot's frozen "now", copied onto every group so
	// time-based rules (progress deadline, drain window) stay pure functions of
	// their input instead of reaching for a live clock.
	ObservedAt time.Time
}

type rule struct {
	name string
	when func(rg replicaGroup) bool
	then func(rg replicaGroup) []Intent
}

// Reconciler buckets the snapshot into replica groups, diffs desired vs
// observed per group, and decides what should change. Pure: no storage
// access just returns Intents which represent action that need to be taken
// to get the wanted deployment state
type Reconciler struct {
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
		// converging: bucket it as outgoing so it flows through drain/reap like a
		// superseded one, instead of holding the health gate forever.
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

	// Leftover slots the current deployment no longer declares — a region
	// dropped between versions. They still need a group, with a zero replica
	// target, so the Reconciler drains them instead of leaking them forever.
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

var rollingCascade = []rule{
	deploymentFrozen,          // status failed → hold everything until unlock
	crashLooping,              // restart_count > restart_max → fail, freeze
	anyHostlessReplicas,       // target lost its host → re-place
	newHealthOpenPastDeadline, // health gate open too long → fail (stalled)
	notAllHealthy,             // newest not yet healthy, within deadline → hold
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

// deploymentFrozen: failed is terminal for the reconciler — hold the whole
// group (no re-fail, no re-placement, no un-freeze via rolloutComplete) until
// a rollback/redeploy re-points the deployment. Sitting at the top of the
// cascade, it carries the "not failed" precondition for every rule below, so
// none of them checks the status itself.
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

// notAllHealthy: holds the group while any current-revision target replica is
// not yet healthy (and within progress_deadline — past it, newHealthOpenPastDeadline
// has already failed the deploy). Gates ramp-up/scale-down/complete until the
// whole target set is healthy. Emits an explicit skip so the group's held state
// is legible downstream instead of an ambiguous empty result.
var notAllHealthy = rule{
	name: "notAllHealthy",
	when: func(rg replicaGroup) bool {
		return healthyTargets(rg) < int32(len(rg.TargetReplicas))
	},
	then: func(rg replicaGroup) []Intent { return []Intent{{Kind: IntentSkip, Group: rg.Desired.Slot}} },
}

// rollingRampUp: fewer healthy target replicas than desired → canary-then-batch.
// The first replica of a revision carries the whole validation risk: a DOA
// version costs one zombie, not N. Once any target proves healthy the version
// is validated — later replicas failing is environmental (host, capacity) and
// other rules own that — so the rest of the deficit comes up in one batch.
var rollingRampUp = rule{
	name: "rollingRampUp",
	when: func(rg replicaGroup) bool {
		return healthyTargets(rg) < rg.Desired.Replicas
	},
	then: func(rg replicaGroup) []Intent {
		// notAllHealthy rule above guarantees every existing target is healthy here,
		// so zero healthy means zero targets it is indicator to us 
		// that this revision is unproven → canary.
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

var rolloutComplete = rule{
	name: "rolloutComplete",
	when: func(rg replicaGroup) bool {
		// Cross-tick idempotency ordering can't carry: already active → converged
		// on some earlier tick, don't re-emit complete forever. (The failed case is
		// deploymentFrozen's job at the top of the cascade.)
		if rg.Desired.Status == domain.DeploymentActive {
			return false
		}
		return healthyTargets(rg) == rg.Desired.Replicas && len(rg.OutgoingReplicas) == 0
	},
	then: func(rg replicaGroup) []Intent { return []Intent{{Kind: IntentComplete, Group: rg.Desired.Slot}} },
}

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

// reapFailedOutgoing: a failed outgoing replica is a dead container from a
// superseded revision — it serves nothing, drains nothing, yet still holds its
// host reservation and blocks the len(outgoing)==0 convergence checks
// (rolloutComplete, recreate's lease-free gate). Destroy it directly: no drain
// window, there is no traffic to bleed off a crashed container.
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

// no canary, previous version already destroyed, no risk in creating new version
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

var recreateScaleDown = rule{name: "recreateScaleDown", when: rollingScaleDown.when, then: rollingScaleDown.then}

var recreateComplete = rule{name: "recreateComplete", when: rolloutComplete.when, then: rolloutComplete.then}
