package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
)

// reconcileInterval is the idle gap between reconcile passes, not a fixed
// cadence: a pass runs to completion, then the loop waits this long before the
// next. Level-triggered, so a slow or skipped pass just self-heals on the next.
// TODO: source from config once the engine wires it.
const reconcileInterval = 2 * time.Second

type SchedulerStore interface {
	WithReadTx(ctx context.Context, fn func(storage.SnapshotReader) error) error
	WithReconcileTx(ctx context.Context, fn func(storage.ReconcileTx) error) error
}

type Scheduler struct {
	store SchedulerStore
}

func NewScheduler(store SchedulerStore) *Scheduler {
	return &Scheduler{store: store}
}

type stateSnapshot struct {
	desired  []db.SnapshotDesiredRow
	replicas []db.ListActiveReplicasRow
	hosts    []db.Host
	volumes  []db.Volume
}

// tick runs reconcile passes until ctx is cancelled. One pass runs fully before
// the next starts (single goroutine, sequential) — passes never overlap — and
// the loop idles reconcileInterval between them, breaking promptly on cancel.
func (s *Scheduler) tick(ctx context.Context) error {
	for {
		if err := s.reconcile(ctx); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			slog.Info("scheduler -> done")
			return nil
		case <-time.Tick(reconcileInterval):
		}
	}
}

// reconcile is one pass: read the snapshot, then close the desired/observed gap.
func (s *Scheduler) reconcile(ctx context.Context) error {
	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: load snapshot: %w", err)
	}

	// izracunaj desired hash i trenutni state hash i ako su isti kao prosli tick skipuj tick
	// desired/current  = [diff] -> intent map
	// intent map -> host placement
	// db

	// TODO: group snap.replicas by (service_id, region); split each group on
	// IsCurrent (target rev vs outgoing), feed to the rollout orchestrator to
	// produce intents, bin-pack creates onto snap.hosts, commit via
	// store.WithReconcileTx. See docs/rollout-strategy.md.
	slog.Debug("scheduler -> snapshot",
		"desired", len(snap.desired), "replicas", len(snap.replicas),
		"hosts", len(snap.hosts), "volumes", len(snap.volumes))
	return nil
}

func (s *Scheduler) loadSnapshot(ctx context.Context) (stateSnapshot, error) {
	var snap stateSnapshot
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
		return stateSnapshot{}, err
	}
	return snap, nil
}
