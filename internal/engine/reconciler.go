package engine

import (
	"github.com/google/uuid"

	"conductor/internal/storage/db"
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

type replicaSlot struct {
	EnvironmentServiceID uuid.UUID
	Region               string
}

type replicaGroup struct {
	Desired          db.SnapshotDesiredRow
	TargetReplicas   []db.ListActiveReplicasRow
	OutgoingReplicas []db.ListActiveReplicasRow
}

func replicaGroupKey(s replicaSlot) string {
	return s.EnvironmentServiceID.String() + "_" + s.Region
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
