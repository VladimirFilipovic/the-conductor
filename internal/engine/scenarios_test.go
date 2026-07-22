package engine

// Multi-tick rollout scenarios: the Reconciler is a pure single-tick function,
// so a small simulator applies each tick's intents back onto the snapshot the
// way the actuator would; sensor events and the clock advance explicitly, so a
// scenario reads as a timeline.
//
// TODO: once the Actuator exists, promote these to end-to-end tests (stubbed
// store, real sensor → reconciler → actuator tick); apply() below is the
// contract those tests will hold the real actuator to.

import (
	"slices"
	"testing"
	"time"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

// Graceful window every sim replica carries; scenarios advance past it to reap.
const simDrainSeconds = 30

type sim struct {
	t        *testing.T
	r        *Reconciler
	now      time.Time
	desired  []desiredState
	replicas []replica
}

func newSim(t *testing.T) *sim {
	return &sim{t: t, r: NewReconciler(), now: time.Unix(1_000_000, 0)}
}

func (s *sim) declare(slot replicaSlot, deploymentID uuid.UUID, n int32, stateful bool, status domain.DeploymentStatus) {
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

// seedHealthy plants n established, serving replicas; CreatedAt staggers
// ascending so newest-first ordering is deterministic (last ID is newest).
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

func (s *sim) tick() []Intent {
	s.t.Helper()
	intents := s.r.Reconcile(stateSnapshot{
		observedAt: s.now,
		desired:    s.desired,
		replicas:   s.replicas,
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

// apply is the sim's actuator model. Placement is instant — scheduling is the
// real actuator's concern; modelling it would pad scenarios with assign_host ticks.
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
		case IntentComplete:
			s.slotDesired(it.Group).Status = domain.DeploymentActive
		case IntentAssignHost:
			s.replicaByID(it.ReplicaID).HostID = uuid.New()
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

func (s *sim) advance(d time.Duration) { s.now = s.now.Add(d) }

// loseHost: the host dies and takes the container — unplaced AND back to
// booting. The health high-water mark survives (it passed probes once), so the
// progress-deadline gate doesn't mistake a re-placed veteran for a stalled rollout.
func (s *sim) loseHost(id uuid.UUID) {
	r := s.replicaByID(id)
	r.HostID = uuid.Nil
	r.Healthy = false
	r.Phase = domain.ReplicaPhaseStarting
}

// vanish is the chaos event for a replica wiped without a trace (host
// disk lost, sensor reaped the row): it simply disappears from the snapshot.
func (s *sim) vanish(id uuid.UUID) {
	s.replicas = slices.DeleteFunc(s.replicas, func(r replica) bool { return r.ID == id })
}

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

// The invariant every converged scenario must end on: no replica left hostless.
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

// Two of three replicas vanish without a trace: the survivor keeps the version
// proven, so the WHOLE deficit heals in one tick — no canary re-proof, no
// re-complete (already active). Deficit of two because a single create couldn't
// tell batch from canary.
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

// --- wiring smoke ---

// One snapshot, three unrelated groups, one tick: each group gets exactly its
// own intent and nothing leaks across slots.
func TestScenarioMixedSnapshot(t *testing.T) {
	s := newSim(t)
	statelessSlot := replicaSlot{uuid.New(), "eu-west"}
	statefulSlot := replicaSlot{uuid.New(), "eu-west"}
	orphanSlot := replicaSlot{uuid.New(), "us-east"}

	// Stateless service cold-starting.
	s.declare(statelessSlot, uuid.New(), 2, false, domain.DeploymentPending)
	// Stateful service mid-update: healthy target, old revision still up.
	statefulV := uuid.New()
	s.declare(statefulSlot, statefulV, 1, true, domain.DeploymentDraining)
	s.seedHealthy(statefulSlot, statefulV, 1)
	outgoingID := s.seedOutgoing(statefulSlot)
	// Region nobody declares anymore.
	orphanID := s.seedOutgoing(orphanSlot)

	got := s.tick()
	want := []Intent{
		{Kind: IntentCreate, Group: statelessSlot}, // canary for the cold start
		{Kind: IntentDrain, ReplicaID: outgoingID}, // stateful retires its old side
		{Kind: IntentDrain, ReplicaID: orphanID},   // orphan slot drains its leftovers
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tick intents = %v, want %v", got, want)
	}
}
