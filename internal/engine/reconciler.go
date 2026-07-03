package engine

import (
	"github.com/google/uuid"
)

type IntentKind string

const (
	IntentCreate  IntentKind = "create"
	IntentDrain   IntentKind = "drain"
	IntentDestroy IntentKind = "destroy"
)

type Intent struct {
	Kind      IntentKind
	Group     replicaSlot
	ReplicaID uuid.UUID
}

// replicaSlot is the reconcile grouping key; comparable, so it indexes maps
// directly.
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
}

// Reconciler diffs desired vs observed per replica group and decides what
// should change. Pure: no storage access, so strategies stay unit-testable.
type Reconciler struct{}

func NewReconciler() *Reconciler {
	return &Reconciler{}
}

// TODO: feed each group to the rollout orchestrator to produce intents,
// bin-pack creates onto hosts. See docs/rollout-strategy.md.
func (r *Reconciler) Reconcile(groups []replicaGroup) []Intent {
	return nil
}
