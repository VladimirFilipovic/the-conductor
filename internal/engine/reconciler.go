package engine

import (
	"log/slog"
	"slices"
	"time"

	"conductor/internal/config"
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
	// IntentPlaceVolume binds a hostless volume to a host. Emitted only by the
	// placer — the rules cascade knows lifecycle, not geometry.
	IntentPlaceVolume IntentKind = "place_volume"
)

// Intent is the complete decision record handed to the Actuator: everything a
// commit needs rides on the intent, so the Actuator maps kinds to tx calls
// without re-reading state (it commits, it never decides).
type Intent struct {
	Kind      IntentKind
	Group     replicaSlot
	ReplicaID uuid.UUID
	// HostID is set only by the placer, on assign_host and place_volume
	// intents; zero everywhere else.
	HostID uuid.UUID
	// VolumeID is the volume a place_volume intent binds, the lease to acquire
	// on a stateful assign_host, and the lease to release on destroy.
	VolumeID uuid.UUID
	// DeploymentID targets deployment-level writes: the parent for create, the
	// status flip for fail/complete, and the revision traffic switches to on a
	// SwitchTraffic drain or a complete.
	DeploymentID uuid.UUID
	// Revision is the replica's snapshot revision — the CAS guard on phase
	// writes (drain), so a decision made against a row the Sensor has since
	// moved is dropped instead of clobbering.
	Revision int64
	// CPUMillicores/MemBytes carry the create spec so the Actuator mints the
	// replica row from the intent alone.
	CPUMillicores int32
	MemBytes      int64
	// SwitchTraffic marks a drain that retires a superseded revision: the
	// Actuator commits the slot's whole drain batch plus the served-revision
	// flip to DeploymentID in ONE tx, so traffic leaves the old side atomically
	// with draining it. Scale-down drains never set it — excess replicas of the
	// serving revision retire without moving the pointer.
	SwitchTraffic bool
}

type replicaSlot struct {
	EnvironmentServiceID uuid.UUID
	Region               string
}

type replicaGroup struct {
	Desired desiredState
	// Volume is the service's volume in this slot's region; zero when none.
	// Stateful creates bind it onto the replica row, which is what lets the
	// placer pin the replica to the volume's host and the actuator acquire the
	// single-writer lease.
	Volume           volume
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
	placer   placer // WHERE: fills HostID on assign_host, places hostless volumes
}

func NewReconciler(placement config.Placement) *Reconciler {
	return &Reconciler{
		rolling:  rollingCascade,
		recreate: recreateCascade,
		orphan:   orphanCascade,
		placer:   placer{cfg: placement},
	}
}

func (r *Reconciler) Reconcile(snap stateSnapshot) []Intent {
	intents := r.planIntents(buildReplicaGroups(snap))
	intents = r.placer.placeHostless(snap, intents)
	intents = append(intents, r.placer.placeVolumes(snap)...)
	return intents
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

	type volumeKey struct {
		serviceID uuid.UUID
		region    string
	}
	volumeIndex := make(map[volumeKey]volume, len(snap.volumes))
	for _, v := range snap.volumes {
		volumeIndex[volumeKey{v.ServiceID, v.Region}] = v
	}

	groups := make([]replicaGroup, 0, len(snap.desired))
	for _, d := range snap.desired {
		b := replicaIndex[d.Slot]
		delete(replicaIndex, d.Slot)
		groups = append(groups, replicaGroup{
			Desired:          d,
			Volume:           volumeIndex[volumeKey{d.ServiceID, d.Slot.Region}],
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
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: r.ID, VolumeID: r.VolumeID})
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
		return []Intent{{Kind: IntentFail, Group: rg.Desired.Slot, DeploymentID: rg.Desired.DeploymentID}}
	},
}

var anyHostlessReplicas = rule{
	name: "anyHostlessReplicas",
	when: func(rg replicaGroup) bool {
		return slices.ContainsFunc(rg.TargetReplicas, hostless)
	},
	then: func(rg replicaGroup) []Intent {
		intents := []Intent{}
		for _, rp := range rg.TargetReplicas {
			if hostless(rp) {
				// VolumeID rides along so the actuator (re-)acquires the
				// single-writer lease in the same tx that binds the host.
				intents = append(intents, Intent{Kind: IntentAssignHost, ReplicaID: rp.ID, VolumeID: rp.VolumeID})
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
		return []Intent{{Kind: IntentFail, Group: rg.Desired.Slot, DeploymentID: rg.Desired.DeploymentID}}
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
		// notAllHealthy above guarantees every existing target is healthy here,
		// so zero healthy means zero targets: the revision is unproven → canary.
		n := rg.Desired.Replicas - healthyTargets(rg)
		if healthyTargets(rg) == 0 {
			n = 1
		}
		intents := make([]Intent, n)
		for i := range intents {
			// Full replica spec rides the intent so the actuator mints the row
			// alone (hostless — the placer binds a host next tick).
			intents[i] = Intent{
				Kind:          IntentCreate,
				Group:         rg.Desired.Slot,
				DeploymentID:  rg.Desired.DeploymentID,
				CPUMillicores: rg.Desired.CPUMillicores,
				MemBytes:      rg.Desired.MemBytes,
			}
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
			intents[i] = Intent{Kind: IntentDrain, ReplicaID: sortedReplicas[i].ID, Revision: sortedReplicas[i].Revision}
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
	then: func(rg replicaGroup) []Intent {
		return []Intent{{Kind: IntentComplete, Group: rg.Desired.Slot, DeploymentID: rg.Desired.DeploymentID}}
	},
}

var drainOutgoing = rule{
	name: "drainOutgoing",
	when: func(rg replicaGroup) bool {
		return slices.ContainsFunc(rg.OutgoingReplicas, drainable)
	},
	then: func(rg replicaGroup) []Intent {
		// Retiring a superseded revision is the blue/green traffic-switch
		// moment: the batch carries the current deployment so the actuator
		// flips served_revisions in the same tx. Orphan slots have no current
		// deployment — nothing to switch to, plain drain.
		switchTraffic := rg.Desired.DeploymentID != uuid.Nil
		var intents []Intent
		for _, or := range rg.OutgoingReplicas {
			if drainable(or) {
				intents = append(intents, Intent{
					Kind:          IntentDrain,
					Group:         rg.Desired.Slot,
					ReplicaID:     or.ID,
					Revision:      or.Revision,
					DeploymentID:  rg.Desired.DeploymentID,
					SwitchTraffic: switchTraffic,
				})
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
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: or.ID, VolumeID: or.VolumeID})
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
				intents = append(intents, Intent{Kind: IntentDestroy, ReplicaID: or.ID, VolumeID: or.VolumeID})
			}
		}
		return intents
	},
}

func failedPhase(rp replica) bool { return rp.Phase == domain.ReplicaPhaseFailed }

func hostless(rp replica) bool { return rp.HostID == uuid.Nil }

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
			// Stateful create binds the slot's volume onto the replica row,
			// pinning every downstream decision to it: placement to the
			// volume's host, the single-writer lease.
			intents[i] = Intent{
				Kind:          IntentCreate,
				Group:         rg.Desired.Slot,
				DeploymentID:  rg.Desired.DeploymentID,
				CPUMillicores: rg.Desired.CPUMillicores,
				MemBytes:      rg.Desired.MemBytes,
				VolumeID:      rg.Volume.ID,
			}
		}
		return intents
	},
}

var recreateScaleDown = rule{name: "recreateScaleDown", when: rollingScaleDown.when, then: rollingScaleDown.then}

var recreateComplete = rule{name: "recreateComplete", when: rolloutComplete.when, then: rolloutComplete.then}
