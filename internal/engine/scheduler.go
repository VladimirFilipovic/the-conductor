package engine

import (
	"context"
	"errors"
	"fmt"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
)

type SchedulerStore interface {
	WithReadTx(ctx context.Context, fn func(storage.SnapshotReader) error) error
	WithReconcileTx(ctx context.Context, fn func(storage.ReconcileTx) error) error
}

type Scheduler struct {
	store SchedulerStore
}

// NewScheduler wires the reconcile loop to its persistence. The store is taken as
// the SchedulerStore interface, not a concrete type, so the engine stays
// backend-agnostic — the composition root picks the implementation.
func NewScheduler(store SchedulerStore) *Scheduler {
	return &Scheduler{store: store}
}

// fleetSnapshot is one coherent reconcile-pass view: desired targets and the
// observed fleet, all read at the same REPEATABLE READ instant.
type fleetSnapshot struct {
	desired  []db.SnapshotDesiredRow
	replicas []db.Replica
	hosts    []db.Host
	volumes  []db.Volume
}

// tick runs one reconcile pass: reads desired state, compares against observed
// replicas, and issues placement/lifecycle writes to close the gap.
func (s *Scheduler) tick(ctx context.Context) error {
	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: load snapshot: %w", err)
	}
	_ = snap
	// TODO: diff snap.desired against snap.replicas, bin-pack onto snap.hosts,
	// and commit placements via store.WithReconcileTx.
	return errors.New("not implemented")
}

// loadSnapshot reads the full desired+observed state in one read-only snapshot
// tx so the diff can't tear (desired scaled out from under the replica list).
func (s *Scheduler) loadSnapshot(ctx context.Context) (fleetSnapshot, error) {
	var snap fleetSnapshot
	err := s.store.WithReadTx(ctx, func(r storage.SnapshotReader) error {
		var err error
		if snap.desired, err = r.SnapshotDesired(ctx); err != nil {
			return err
		}
		if snap.replicas, err = r.ListActiveReplicas(ctx); err != nil {
			return err
		}
		if snap.hosts, err = r.ListSchedulableHosts(ctx); err != nil {
			return err
		}
		snap.volumes, err = r.ListActiveVolumes(ctx)
		return err
	})
	if err != nil {
		return fleetSnapshot{}, err
	}
	return snap, nil
}
