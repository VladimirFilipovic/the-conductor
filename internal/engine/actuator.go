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
func (a *Actuator) Apply(ctx context.Context, intents []Intent) error {
	return nil
}
