package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"conductor/internal/storage"
)

// reconcileInterval is the idle gap between reconcile passes, not a fixed
// cadence: a pass runs to completion, then the loop waits this long before the
// next. Level-triggered, so a slow or skipped pass just self-heals on the next.
// TODO: source from config once the engine wires it.
const reconcileInterval = 2 * time.Second

// maxConsecutiveFailures is how many passes may fail back-to-back before the
// engine gives up. A lone failure is logged and retried — transient DB errors
// self-heal on the next tick; only a persistent fault takes the engine down.
const maxConsecutiveFailures = 5

type SnapshotStore interface {
	WithReadTx(ctx context.Context, fn func(storage.SnapshotReader) error) error
}

// Consumer-side views of the pass stages, so tests can fake either
// independently of the concrete Reconciler/Actuator.
type reconciler interface {
	Reconcile(groups []replicaGroup) []Intent
}

type actuator interface {
	Apply(ctx context.Context, intents []Intent) error
}

// Engine drives the reconcile loop: it snapshots state, buckets replicas into
// groups, asks the Reconciler what should change, and hands the resulting
// intents to the Actuator to commit.
type Engine struct {
	store      SnapshotStore
	reconciler reconciler
	actuator   actuator
}

func New(store SnapshotStore, r reconciler, a actuator) *Engine {
	return &Engine{store: store, reconciler: r, actuator: a}
}

// run drives reconcile passes until ctx is cancelled. One pass runs fully
// before the next starts — passes never overlap. Blocking.
func (e *Engine) run(ctx context.Context) error {
	// Timer over Ticker: re-armed after each pass so the interval is a true
	// idle gap — a pass slower than the interval can't stack a buffered tick
	// and turn the loop into a busy spin.
	timer := time.NewTimer(reconcileInterval)
	defer timer.Stop()

	failures := 0
	for {
		switch err := e.reconcile(ctx); {
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			slog.Info("engine -> done")
			return nil
		default:
			failures++
			if failures >= maxConsecutiveFailures {
				return fmt.Errorf("engine: %d consecutive failed passes: %w", failures, err)
			}
			slog.Warn("engine: reconcile pass failed, retrying next tick",
				"err", err, "consecutive", failures)
		}

		timer.Reset(reconcileInterval)
		select {
		case <-ctx.Done():
			slog.Info("engine -> done")
			return nil
		case <-timer.C:
		}
	}
}

// reconcile is one pass: read the snapshot, then close the desired/observed gap.
func (e *Engine) reconcile(ctx context.Context) error {
	snap, err := e.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("engine: load snapshot: %w", err)
	}

	groups := buildReplicaGroups(snap)
	intents := e.reconciler.Reconcile(groups)

	if err := e.actuator.Apply(ctx, intents); err != nil {
		return fmt.Errorf("engine: apply intents: %w", err)
	}

	slog.Debug("engine -> snapshot",
		"desired", len(snap.desired), "replicas", len(snap.replicas),
		"hosts", len(snap.hosts), "volumes", len(snap.volumes))
	return nil
}

func buildReplicaGroups(snap stateSnapshot) []replicaGroup {
	type replicaBucket struct {
		target   []replica
		outgoing []replica
	}
	replicaIndex := make(map[replicaSlot]replicaBucket, len(snap.replicas))
	for _, r := range snap.replicas {
		b := replicaIndex[r.Slot]
		if r.Current {
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
		})
	}
	return groups
}
