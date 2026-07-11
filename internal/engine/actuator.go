package engine

import (
	"context"

	"conductor/internal/storage"
)

type ActuatorStore interface {
	WithReconcileTx(ctx context.Context, fn func(storage.ReconcileTx) error) error
}

// Actuator commits intents to storage. It makes no decisions — it only applies
// what the Reconciler produced, losing CAS races safely (ErrConflict = another
// pass already moved the row; the next tick re-reads and self-heals).
type Actuator struct {
	store ActuatorStore
}

func NewActuator(store ActuatorStore) *Actuator {
	return &Actuator{store: store}
}

// TODO: commit each intent via store.WithReconcileTx.
//
// Blue/green traffic switch: when applying a drain batch that retires a slot's
// outgoing side (the first IntentDrain set after the target set went fully
// healthy — docs #4 "shift traffic → drain"), the same tx must also call
// ReconcileTx.SetServedRevision(slot, newDeploymentID). One commit = pointer
// flip + old side draining, so the router never observes neither or both
// revisions serving. Scale-down drains of the served revision itself must NOT
// touch the pointer.
func (a *Actuator) Apply(ctx context.Context, intents []Intent) error {
	return nil
}
