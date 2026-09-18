package engine

// Multi-tick rollout scenarios through Reconcile. The Reconciler is a pure
// single-tick function, so these tests drive it with a small simulator: each
// tick reconciles the current snapshot and applies the resulting intents back
// onto it the way the actuator would (create → new booting replica, drain →
// DrainedAt stamp, destroy → gone from the snapshot). Sensor events (health
// probes, crashes) and the clock are advanced explicitly between ticks, so a
// scenario reads as a timeline.
//
// These stay decision-level on purpose: they assert the intent stream, with a
// fake clock, so timing rules (drain windows, progress deadlines) are testable
// without waiting. The real end-to-end coverage — actual engine tick over a
// stub store, sensor → snapshot → reconciler → actuator, asserting committed
// state — lives in e2e_test.go.

import (
	"slices"
	"testing"
	"time"

	"conductor/internal/config"
	"conductor/internal/domain"

	"github.com/google/uuid"
)

// simDrainSeconds is the graceful window every sim replica carries; scenarios
// advance past it with advance(simDrainSeconds*time.Second + ...).
const simDrainSeconds = 30

type sim struct {
	t        *testing.T
	r        *Reconciler
	now      time.Time
	desired  []desiredState
	replicas []replica
	hosts    []host
}

func newSim(t *testing.T) *sim {
	return &sim{t: t, r: NewReconciler(config.DefaultPlacement()), now: time.Unix(1_000_000, 0)}
}

func (s *sim) declare(slot replicaSlot, deploymentID uuid.UUID, n int32, stateful bool, status domain.DeploymentStatus) {
	s.ensureHost(slot.Region)
	s.desired = append(s.desired, desiredState{
		Slot:             slot,
		DeploymentID:     deploymentID,
		Replicas:         n,
		RestartMax:       3,
		ProgressDeadline: 60,
		Status:           status,
		Stateful:         stateful,
	})
}

// seedHealthy plants n established, serving replicas for deploymentID.
// CreatedAt is staggered ascending so "newest first" ordering is
// deterministic: the last returned ID is the newest.
func (s *sim) seedHealthy(slot replicaSlot, deploymentID uuid.UUID, n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		born := s.now.Add(-time.Hour + time.Duration(i)*time.Minute)
		ids[i] = uuid.New()
		s.replicas = append(s.replicas, replica{
			ID:                   ids[i],
			DeploymentID:         deploymentID,
			Slot:                 slot,
			HostID:               uuid.New(),
			Phase:                domain.ReplicaPhaseActive,
			Healthy:              true,
			Current:              true,
			DrainSeconds:         simDrainSeconds,
			CreatedAt:            born,
			HealthChecksPassedAt: born,
		})
	}
	return ids
}

// seedOutgoing plants a still-serving replica of a superseded deployment.
func (s *sim) seedOutgoing(slot replicaSlot) uuid.UUID {
	id := uuid.New()
	s.replicas = append(s.replicas, replica{
		ID:           id,
		DeploymentID: uuid.New(),
		Slot:         slot,
		HostID:       uuid.New(),
		Phase:        domain.ReplicaPhaseActive,
		Healthy:      true,
		Current:      false,
		DrainSeconds: simDrainSeconds,
		CreatedAt:    s.now.Add(-2 * time.Hour),
	})
	return id
}

// ensureHost gives the region one huge host so the placer has somewhere to
// put re-placed replicas — geometry stays out of lifecycle scenarios.
func (s *sim) ensureHost(region string) {
	for _, h := range s.hosts {
		if h.Region == region {
			return
		}
	}
	s.hosts = append(s.hosts, host{
		ID:            uuid.New(),
		Region:        region,
		CPUMillicores: 1 << 20,
		MemBytes:      1 << 40,
		DiskBytes:     1 << 40,
		Open:          true,
	})
}

// tick reconciles the current snapshot and applies the intents, mimicking one
// engine pass.
func (s *sim) tick() []Intent {
	s.t.Helper()
	intents := s.r.Reconcile(stateSnapshot{
		observedAt: s.now,
		desired:    s.desired,
		replicas:   s.replicas,
		hosts:      s.hosts,
	})
	s.apply(intents)
	return intents
}

// tickExpect asserts the exact intent-kind sequence of one tick — including
// the empty call for steady-state ticks — so absences (e.g. recreate's
// no-surge "no create while outgoing lives") are proven, not implied.
func (s *sim) tickExpect(want ...IntentKind) []Intent {
	s.t.Helper()
	got := s.tick()
	kinds := make([]IntentKind, len(got))
	for i, it := range got {
		kinds[i] = it.Kind
	}
	if !slices.Equal(kinds, want) {
		s.t.Fatalf("tick intents = %v, want %v", kinds, want)
	}
	return got
}

// apply is the sim's actuator model. Placement is instant (created replicas
// get a host immediately) — scheduling is the real actuator's concern, and
// modelling it here would just pad every scenario with assign_host ticks.
func (s *sim) apply(intents []Intent) {
	s.t.Helper()
	for _, it := range intents {
		switch it.Kind {
		case IntentCreate:
			d := s.slotDesired(it.Group)
			s.replicas = append(s.replicas, replica{
				ID:           uuid.New(),
				DeploymentID: d.DeploymentID,
				Slot:         it.Group,
				HostID:       uuid.New(),
				Phase:        domain.ReplicaPhaseStarting,
				Current:      true,
				DrainSeconds: simDrainSeconds,
				CreatedAt:    s.now,
			})
		case IntentDrain:
			r := s.replicaByID(it.ReplicaID)
			r.Phase = domain.ReplicaPhaseDraining
			r.DrainedAt = s.now
		case IntentDestroy:
			s.replicas = slices.DeleteFunc(s.replicas, func(r replica) bool { return r.ID == it.ReplicaID })
		case IntentFail:
			s.slotDesired(it.Group).Status = domain.DeploymentFailed
		case IntentFreezeReplica:
			r := s.replicaByID(it.ReplicaID)
			r.Phase = domain.ReplicaPhaseFailed
			r.Healthy = false
		case IntentComplete:
			s.slotDesired(it.Group).Status = domain.DeploymentActive
		case IntentAssignHost:
			s.replicaByID(it.ReplicaID).HostID = it.HostID
		case IntentSkip:
		}
	}
}

// markHealthy is the sensor event: every live current replica passes its probe.
func (s *sim) markHealthy() {
	for i := range s.replicas {
		r := &s.replicas[i]
		if !r.Current || !r.DrainedAt.IsZero() || r.Phase.Terminal() {
			continue
		}
		r.Healthy = true
		r.Phase = domain.ReplicaPhaseActive
		if r.HealthChecksPassedAt.IsZero() {
			r.HealthChecksPassedAt = s.now
		}
	}
}

// crashPastBudget is the sensor event for a crash loop: every current replica
// of the slot blows through its restart budget.
func (s *sim) crashPastBudget(slot replicaSlot) {
	max := s.slotDesired(slot).RestartMax
	for i := range s.replicas {
		if s.replicas[i].Slot == slot && s.replicas[i].Current {
			s.replicas[i].RestartCount = max + 1
		}
	}
}

// crashReplicaPastBudget is the same event for one replica: what a bad host
// or a poisoned config on a single container looks like under a fleet that
// is otherwise fine.
func (s *sim) crashReplicaPastBudget(id uuid.UUID) {
	r := s.replicaByID(id)
	r.RestartCount = s.slotDesired(r.Slot).RestartMax + 1
	r.Healthy = false
	r.Phase = domain.ReplicaPhaseStarting
}

func (s *sim) assertPhase(id uuid.UUID, want domain.ReplicaPhase) {
	s.t.Helper()
	if got := s.replicaByID(id).Phase; got != want {
		s.t.Fatalf("replica %s phase = %s, want %s", id, got, want)
	}
}

func (s *sim) advance(d time.Duration) { s.now = s.now.Add(d) }

// loseHost is the chaos event for a died/drained host: the replica's container
// is gone with it, so it drops to hostless replacing — what MarkHostDown
// writes. Its health high-water mark survives (it did pass probes once), so
// the progress-deadline gate does not mistake a re-placed veteran for a
// stalled rollout.
func (s *sim) loseHost(id uuid.UUID) {
	r := s.replicaByID(id)
	r.HostID = uuid.Nil
	r.Healthy = false
	r.Phase = domain.ReplicaPhaseReplacing
}

// vanish is the chaos event for a replica wiped without a trace (host
// disk lost, sensor reaped the row): it simply disappears from the snapshot.
func (s *sim) vanish(id uuid.UUID) {
	s.replicas = slices.DeleteFunc(s.replicas, func(r replica) bool { return r.ID == id })
}

// markUnhealthy is the sensor event for a failing probe on a live replica.
func (s *sim) markUnhealthy(id uuid.UUID) {
	s.replicaByID(id).Healthy = false
}

func (s *sim) currentIDs(slot replicaSlot) []uuid.UUID {
	var ids []uuid.UUID
	for _, r := range s.replicas {
		if r.Slot == slot && r.Current {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// deployNew commits a new revision for the slot: desired re-points to a fresh
// deployment and every existing replica becomes outgoing.
func (s *sim) deployNew(slot replicaSlot) uuid.UUID {
	id := uuid.New()
	d := s.slotDesired(slot)
	d.DeploymentID = id
	d.Status = domain.DeploymentPending
	for i := range s.replicas {
		if s.replicas[i].Slot == slot {
			s.replicas[i].Current = false
		}
	}
	return id
}

// rollbackTo re-points the slot at a prior deployment: its replicas become
// the target again, everything else outgoing.
func (s *sim) rollbackTo(slot replicaSlot, deploymentID uuid.UUID) {
	d := s.slotDesired(slot)
	d.DeploymentID = deploymentID
	d.Status = domain.DeploymentDraining
	for i := range s.replicas {
		if s.replicas[i].Slot == slot {
			s.replicas[i].Current = s.replicas[i].DeploymentID == deploymentID
		}
	}
}

func (s *sim) scale(slot replicaSlot, n int32) { s.slotDesired(slot).Replicas = n }

// dropSlot removes the slot from desired (region dropped between versions);
// its replicas turn outgoing, as their deployment no longer declares them.
func (s *sim) dropSlot(slot replicaSlot) {
	s.desired = slices.DeleteFunc(s.desired, func(d desiredState) bool { return d.Slot == slot })
	for i := range s.replicas {
		if s.replicas[i].Slot == slot {
			s.replicas[i].Current = false
		}
	}
}

func (s *sim) slotDesired(slot replicaSlot) *desiredState {
	for i := range s.desired {
		if s.desired[i].Slot == slot {
			return &s.desired[i]
		}
	}
	s.t.Fatalf("no desired state for slot %v", slot)
	return nil
}

func (s *sim) replicaByID(id uuid.UUID) *replica {
	for i := range s.replicas {
		if s.replicas[i].ID == id {
			return &s.replicas[i]
		}
	}
	s.t.Fatalf("no replica %s", id)
	return nil
}

func (s *sim) assertStatus(slot replicaSlot, want domain.DeploymentStatus) {
	s.t.Helper()
	if got := s.slotDesired(slot).Status; got != want {
		s.t.Fatalf("deployment status = %s, want %s", got, want)
	}
}

// assertAllPlaced: the invariant every converged scenario must end on — no
// replica left without a host.
func (s *sim) assertAllPlaced(slot replicaSlot) {
	s.t.Helper()
	for _, r := range s.replicas {
		if r.Slot == slot && r.HostID == uuid.Nil {
			s.t.Fatalf("replica %s left hostless", r.ID)
		}
	}
}

func (s *sim) assertFleet(slot replicaSlot, deploymentID uuid.UUID, n int) {
	s.t.Helper()
	count := 0
	for _, r := range s.replicas {
		if r.Slot == slot && r.DeploymentID == deploymentID {
			count++
		}
	}
	if count != n {
		s.t.Fatalf("fleet of deployment %s = %d replicas, want %d", deploymentID, count, n)
	}
}

// --- stateless (rolling) scenarios ---

func TestScenarioColdStart(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 3, false, domain.DeploymentPending)

	s.tickExpect(IntentCreate) // unproven revision risks exactly one canary
	s.tickExpect(IntentSkip)   // canary still booting: health gate holds
	s.markHealthy()
	s.tickExpect(IntentCreate, IntentCreate) // version proven: batch the deficit
	s.markHealthy()
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v1, 3)
	s.assertAllPlaced(slot)
	s.tickExpect() // steady state
}

func TestScenarioScaleUp(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	s.seedHealthy(slot, v1, 2)

	s.scale(slot, 4)
	s.tickExpect(IntentCreate, IntentCreate) // proven version: whole deficit, no canary
	s.markHealthy()
	s.tickExpect() // already active — no re-complete, straight to steady state
	s.assertFleet(slot, v1, 4)
}

func TestScenarioScaleDown(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 3, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 3)

	s.scale(slot, 1)
	drains := s.tickExpect(IntentDrain, IntentDrain)
	// Newest-first: the two youngest replicas are sacrificed.
	if drains[0].ReplicaID != ids[2] || drains[1].ReplicaID != ids[1] {
		t.Fatalf("drained %v/%v, want newest %v then %v", drains[0].ReplicaID, drains[1].ReplicaID, ids[2], ids[1])
	}
	s.tickExpect() // drain window still open
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy, IntentDestroy)
	s.assertFleet(slot, v1, 1)
	s.tickExpect() // steady state
}

func TestScenarioRollingUpdate(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	s.seedHealthy(slot, v1, 2)

	v2 := s.deployNew(slot)
	s.tickExpect(IntentCreate) // canary first — v1 keeps serving untouched (create-before-retire)
	s.markHealthy()
	s.tickExpect(IntentCreate) // canary proved v2: batch the rest
	s.markHealthy()
	s.tickExpect(IntentDrain, IntentDrain) // target side fully healthy: retire v1
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy, IntentDestroy)
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v2, 2)
	s.assertFleet(slot, v1, 0)
	s.assertAllPlaced(slot)
	s.tickExpect() // steady state
}

// The availability story: v2 dies, the deployment freezes, and v1 — still
// outgoing, still serving — is never drained. A failed deploy is not an outage.
func TestScenarioFailedDeployRollback(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	s.seedHealthy(slot, v1, 2)

	v2 := s.deployNew(slot)
	s.tickExpect(IntentCreate) // v2 canary
	s.crashPastBudget(slot)    // canary crashloops
	s.tickExpect(IntentFail)
	s.assertStatus(slot, domain.DeploymentFailed)
	s.tickExpect(IntentSkip) // frozen: no drains of v1, no retry of v2
	s.assertFleet(slot, v1, 2)

	s.rollbackTo(slot, v1)
	s.tickExpect(IntentDrain) // v2 canary is outgoing now — retire it
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy)
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v1, 2)
	s.assertFleet(slot, v2, 0)
	s.tickExpect() // steady state
}

func TestScenarioStalledRollout(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	s.declare(slot, uuid.New(), 1, false, domain.DeploymentPending)

	s.tickExpect(IntentCreate) // canary
	s.tickExpect(IntentSkip)   // boots but never goes healthy
	s.advance(61 * time.Second)
	s.tickExpect(IntentFail) // progress deadline breached: stalled
	s.assertStatus(slot, domain.DeploymentFailed)
	s.tickExpect(IntentSkip) // frozen
}

func TestScenarioOrphanRegionDrained(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "us-east"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	s.seedHealthy(slot, v1, 2)

	s.dropSlot(slot)
	s.tickExpect(IntentDrain, IntentDrain)
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy, IntentDestroy)
	s.assertFleet(slot, v1, 0)
	s.tickExpect() // nothing left to reconcile
}

// --- stateful (recreate) scenarios ---

// Retire-before-create: the single-writer volume lease forbids surge, so v1
// must be fully reaped before any v2 replica exists. tickExpect asserts exact
// kinds, so the absence of IntentCreate on the drain/reap ticks is proven.
func TestScenarioRecreateUpdate(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 1, true, domain.DeploymentActive)
	s.seedHealthy(slot, v1, 1)

	v2 := s.deployNew(slot)
	s.tickExpect(IntentDrain) // old side first — and no create alongside
	s.tickExpect()            // drain window open: still no create
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy) // v1 reaped, lease free
	s.tickExpect(IntentCreate)  // only now does v2 come up
	s.markHealthy()
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v2, 1)
	s.tickExpect() // steady state
}

func TestScenarioStatefulColdStart(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, true, domain.DeploymentPending)

	s.tickExpect(IntentCreate, IntentCreate) // no canary: whole batch at once
	s.markHealthy()
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v1, 2)
	s.tickExpect() // steady state
}

// Lease release itself is the actuator's job when the drained replica reaps;
// the reconciler's part is drain newest-first, then destroy.
func TestScenarioStatefulScaleDown(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, true, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)

	s.scale(slot, 1)
	drains := s.tickExpect(IntentDrain)
	if drains[0].ReplicaID != ids[1] {
		t.Fatalf("drained %v, want newest %v", drains[0].ReplicaID, ids[1])
	}
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy)
	s.assertFleet(slot, v1, 1)
	s.tickExpect() // steady state
}

// --- chaos scenarios: the world breaks between ticks ---

// A host dies under a steady fleet: re-place first (assign_host), then the
// health gate holds the group while the container reboots — no panic creates,
// no drains of the survivor.
func TestScenarioHostLossRecovery(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)

	s.loseHost(ids[1])
	s.tickExpect(IntentAssignHost)
	s.tickExpect(IntentSkip) // re-placed but rebooting: health gate holds
	s.markHealthy()
	s.tickExpect() // steady state
	s.assertAllPlaced(slot)
	s.assertFleet(slot, v1, 2)
}

// The canary loses its host mid-rollout: placement sits above ramp-up in the
// cascade, so the fix is re-placing the existing canary — NOT minting a second
// one. The exact-kind assert proves no create sneaks in alongside.
func TestScenarioHostLossDuringRollout(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentPending)

	s.tickExpect(IntentCreate) // canary
	s.loseHost(s.currentIDs(slot)[0])
	s.tickExpect(IntentAssignHost) // re-place, no second canary
	s.tickExpect(IntentSkip)       // booting again
	s.markHealthy()
	s.tickExpect(IntentCreate) // canary proven → batch the deficit
	s.markHealthy()
	s.tickExpect(IntentComplete)
	s.assertAllPlaced(slot)
	s.assertFleet(slot, v1, 2)
	s.tickExpect() // steady state
}

// Two of three replicas vanish without a trace (host disk gone, sensor reaped
// the rows): the fleet self-heals with the WHOLE deficit in one tick — one
// survivor keeps the version proven, so no canary re-proof (which would emit a
// single create and wait). No re-complete either: the deployment is already
// active. A deficit of one couldn't tell batch from canary — both emit one
// create — hence two.
func TestScenarioReplicaVanishesSelfHeals(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 3, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 3)

	s.vanish(ids[0])
	s.vanish(ids[1])
	s.tickExpect(IntentCreate, IntentCreate) // batch, not canary
	s.markHealthy()
	s.tickExpect() // steady state, still active — no complete re-emit
	s.assertFleet(slot, v1, 3)
	s.assertAllPlaced(slot)
}

// A probe blip on one replica must produce exactly a hold — no replacement
// create, no drain, no fail. The health gate absorbs transient unhealth.
func TestScenarioFlappingReplicaOnlyHolds(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)

	s.markUnhealthy(ids[0])
	s.tickExpect(IntentSkip) // hold, nothing else
	s.tickExpect(IntentSkip) // still flapping, still just a hold
	s.markHealthy()
	s.tickExpect() // recovered: steady state
	s.assertFleet(slot, v1, 2)
}

// --- frozen replica: crash loop under an ACTIVE deployment ---

// One replica of a serving fleet blows its restart budget. The deployment is
// not failed — it was proven — so the offender is frozen alone and the group
// runs degraded: no replacement (a clone would crash the same way), no drain
// of the survivor, status stays active. Scaling down spends the excess on the
// frozen replica first (destroy, no drain window); scaling back up only mints
// a new one once the frozen slot is gone.
func TestScenarioActiveCrashLoopDegrades(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)

	s.crashReplicaPastBudget(ids[1])
	frozen := s.tickExpect(IntentFreezeReplica)
	if frozen[0].ReplicaID != ids[1] {
		t.Fatalf("froze %s, want the offender %s", frozen[0].ReplicaID, ids[1])
	}
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertPhase(ids[1], domain.ReplicaPhaseFailed)
	s.tickExpect() // degraded steady state: no create, no drain, no fail
	s.tickExpect()
	s.assertFleet(slot, v1, 2)

	s.scale(slot, 1)
	gone := s.tickExpect(IntentDestroy) // the frozen one is the excess
	if gone[0].ReplicaID != ids[1] {
		t.Fatalf("scale-down destroyed %s, want the frozen %s", gone[0].ReplicaID, ids[1])
	}
	s.assertFleet(slot, v1, 1)
	s.tickExpect()

	s.scale(slot, 2)
	s.tickExpect(IntentCreate) // proven version, real deficit: one replacement
	s.markHealthy()
	s.tickExpect()
	s.assertFleet(slot, v1, 2)
	s.assertStatus(slot, domain.DeploymentActive)
}

// The manual "replace once": deleting the frozen row (DELETE /v1/replicas/{id})
// is the only automatic replacement path — the deficit becomes visible and
// rampUp fills it.
func TestScenarioFrozenReplicaDeletedIsReplaced(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)

	s.crashReplicaPastBudget(ids[0])
	s.tickExpect(IntentFreezeReplica)
	s.tickExpect()

	s.vanish(ids[0])
	s.tickExpect(IntentCreate)
	s.markHealthy()
	s.tickExpect()
	s.assertFleet(slot, v1, 2)
}

// A redeploy thaws by supersession: the frozen replica turns outgoing and
// reapFailedOutgoing reclaims it, while v2 rolls out through the ordinary
// canary path. The frozen row never blocks completion.
func TestScenarioFrozenReplicaRedeployReaps(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)
	s.crashReplicaPastBudget(ids[1])
	s.tickExpect(IntentFreezeReplica)

	v2 := s.deployNew(slot)
	s.tickExpect(IntentCreate) // v2 canary; the live v1 keeps serving
	s.markHealthy()
	s.tickExpect(IntentCreate) // proven: batch the rest
	s.markHealthy()
	drains := s.tickExpect(IntentDrain) // only the LIVE v1 drains; the frozen one is terminal
	if drains[0].ReplicaID != ids[0] {
		t.Fatalf("drained %s, want the live v1 %s", drains[0].ReplicaID, ids[0])
	}
	reaps := s.tickExpect(IntentDestroy) // reapFailedOutgoing: frozen v1 reclaimed during the drain window
	if reaps[0].ReplicaID != ids[1] {
		t.Fatalf("reaped %s, want the frozen v1 %s", reaps[0].ReplicaID, ids[1])
	}
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy)
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertFleet(slot, v2, 2)
	s.assertFleet(slot, v1, 0)
}

// Rolling back onto a revision that still carries a frozen replica must not
// strand the deployment in draining: the held slot counts as converged, so
// the rollback completes into the same active-but-degraded state the freeze
// left behind.
func TestScenarioRollbackOntoFrozenCompletesDegraded(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 2, false, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 2)
	s.crashReplicaPastBudget(ids[1])
	s.tickExpect(IntentFreezeReplica)

	s.deployNew(slot)
	s.tickExpect(IntentCreate) // v2 canary
	s.crashPastBudget(slot)    // canary crashloops → rollout verdict, not a freeze
	s.tickExpect(IntentFail)
	s.tickExpect(IntentSkip) // frozen deployment: v1 (live + frozen) untouched

	s.rollbackTo(slot, v1)
	s.tickExpect(IntentDrain) // retire the v2 canary
	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy)
	s.tickExpect(IntentComplete)
	s.assertStatus(slot, domain.DeploymentActive)
	s.assertPhase(ids[1], domain.ReplicaPhaseFailed) // still frozen, still degraded
	s.assertFleet(slot, v1, 2)
	s.tickExpect()
}

// Stateful: the frozen replica keeps the volume lease with its slot, so
// recreate never mints a replacement that would lose AcquireVolumeLease every
// tick. Deleting the row frees both.
func TestScenarioStatefulFrozenHoldsLease(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu-west"}
	v1 := uuid.New()
	s.declare(slot, v1, 1, true, domain.DeploymentActive)
	ids := s.seedHealthy(slot, v1, 1)

	s.crashReplicaPastBudget(ids[0])
	s.tickExpect(IntentFreezeReplica)
	s.assertStatus(slot, domain.DeploymentActive)
	s.tickExpect() // no create: slot and lease still held
	s.tickExpect()

	s.vanish(ids[0])
	s.tickExpect(IntentCreate)
	s.markHealthy()
	s.tickExpect()
	s.assertFleet(slot, v1, 1)
}

// --- wiring smoke ---

// One snapshot, three unrelated groups, one tick: each group gets exactly its
// own intent and nothing leaks across slots.
func TestScenarioMixedSnapshot(t *testing.T) {
	s := newSim(t)
	statelessSlot := replicaSlot{uuid.New(), "eu-west"}
	statefulSlot := replicaSlot{uuid.New(), "eu-west"}
	orphanSlot := replicaSlot{uuid.New(), "us-east"}

	// Stateless service cold-starting.
	statelessV := uuid.New()
	s.declare(statelessSlot, statelessV, 2, false, domain.DeploymentPending)
	// Stateful service mid-update: healthy target, old revision still up.
	statefulV := uuid.New()
	s.declare(statefulSlot, statefulV, 1, true, domain.DeploymentDraining)
	s.seedHealthy(statefulSlot, statefulV, 1)
	outgoing := s.seedOutgoing(statefulSlot)
	// Region nobody declares anymore.
	orphan := s.seedOutgoing(orphanSlot)

	got := s.tick()
	want := []Intent{
		// canary for the cold start
		{Kind: IntentCreate, Group: statelessSlot, DeploymentID: statelessV},
		// stateful retires its old side — the traffic-switch moment
		{Kind: IntentDrain, Group: statefulSlot, ReplicaID: outgoing, DeploymentID: statefulV, SwitchTraffic: true},
		// orphan slot drains its leftovers; no current deployment, no switch
		{Kind: IntentDrain, Group: orphanSlot, ReplicaID: orphan},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tick intents = %v, want %v", got, want)
	}
}

// drainHostOf is the operator's drain seen from the snapshot: every listed
// replica now sits on a draining host.
func (s *sim) drainHostOf(ids ...uuid.UUID) {
	for _, id := range ids {
		s.replicaByID(id).HostDraining = true
	}
}

// Host drain is surge-then-shrink through the ordinary rolling rules: the
// replicas on the draining host stop counting as capacity, so ramp-up builds
// their replacements (canary first) while they keep serving; only once every
// replacement is healthy does scale-down retire them — draining-host first.
// The group never dips below desired.
func TestDrainHostEvacuatesStatelessSurgeFirst(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu"}
	dep := uuid.New()
	s.declare(slot, dep, 3, false, domain.DeploymentActive)
	old := s.seedHealthy(slot, dep, 3)
	s.drainHostOf(old...)

	s.tickExpect(IntentCreate) // canary: no counted capacity, version re-proven with one
	s.tickExpect(IntentSkip)   // notAllHealthy holds on the booting canary
	s.markHealthy()
	s.tickExpect(IntentCreate, IntentCreate) // the rest of the deficit in one batch
	s.tickExpect(IntentSkip)
	s.markHealthy()

	drains := s.tickExpect(IntentDrain, IntentDrain, IntentDrain)
	for _, it := range drains {
		if !slices.Contains(old, it.ReplicaID) {
			t.Fatalf("scale-down drained %s, a fresh replacement — must retire the draining host's replicas", it.ReplicaID)
		}
	}
	var serving int
	for _, r := range s.replicas {
		if r.Healthy && !r.HostDraining && r.DrainedAt.IsZero() {
			serving++
		}
	}
	if serving != 3 {
		t.Fatalf("serving replacements after evacuation = %d, want 3", serving)
	}

	s.advance(simDrainSeconds*time.Second + time.Second)
	s.tickExpect(IntentDestroy, IntentDestroy, IntentDestroy)
	s.tickExpect()
}

// A single replica on a draining host still gets a replacement before it is
// retired: the count of desired is never the count of what is leaving.
func TestDrainHostSingleReplicaSurgesBeforeRetiring(t *testing.T) {
	s := newSim(t)
	slot := replicaSlot{uuid.New(), "eu"}
	dep := uuid.New()
	s.declare(slot, dep, 1, false, domain.DeploymentActive)
	old := s.seedHealthy(slot, dep, 1)
	s.drainHostOf(old...)

	s.tickExpect(IntentCreate)
	s.tickExpect(IntentSkip)
	s.markHealthy()
	drain := s.tickExpect(IntentDrain)
	if drain[0].ReplicaID != old[0] {
		t.Fatalf("drained %s, want the draining host's replica %s", drain[0].ReplicaID, old[0])
	}
}
