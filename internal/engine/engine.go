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

type SnapshotStore interface {
	WithReadTx(ctx context.Context, fn func(storage.SnapshotReader) error) error
}

// Engine drives the reconcile loop: it snapshots state, buckets replicas into
// groups, asks the Reconciler what should change, and hands the resulting
// intents to the Actuator to commit.
type Engine struct {
	store      SnapshotStore
	reconciler *Reconciler
	actuator   *Actuator
}

type stateSnapshot struct {
	desired  []db.SnapshotDesiredRow
	replicas []db.ListActiveReplicasRow
	hosts    []db.Host
	volumes  []db.Volume
}

func NewEngine(store SnapshotStore, reconciler *Reconciler, actuator *Actuator) *Engine {
	return &Engine{store: store, reconciler: reconciler, actuator: actuator}
}

func Run(ctx context.Context, e *Engine, sensor *Sensor) error {
	slog.Info("engine starting")

	// TODO: rerun up to 3 times on errors, restart the counter
	go sensor.tick(ctx)
	go e.tick(ctx)

	<-ctx.Done()

	slog.Info("engine shutting down")
	return nil
}

// tick runs reconcile passes until ctx is cancelled. One pass runs fully before
// the next starts (single goroutine, sequential) — passes never overlap — and
// the loop idles reconcileInterval between them, breaking promptly on cancel.
func (e *Engine) tick(ctx context.Context) error {
	for {
		if err := e.reconcile(ctx); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			slog.Info("engine -> done")
			return nil
		case <-time.Tick(reconcileInterval):
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
		target   []db.ListActiveReplicasRow
		outgoing []db.ListActiveReplicasRow
	}
	replicaIndex := make(map[string]replicaBucket, len(snap.replicas))
	for _, ri := range snap.replicas {
		key := replicaGroupKey(replicaSlot{ri.EnvironmentServiceID, ri.Region})
		b := replicaIndex[key]
		if ri.IsCurrent {
			b.target = append(b.target, ri)
		} else {
			b.outgoing = append(b.outgoing, ri)
		}
		replicaIndex[key] = b
	}

	groups := make([]replicaGroup, 0, len(snap.desired))
	for _, d := range snap.desired {
		b := replicaIndex[replicaGroupKey(replicaSlot{d.EnvironmentServiceID, d.Region})]
		groups = append(groups, replicaGroup{
			Desired:          d,
			TargetReplicas:   b.target,
			OutgoingReplicas: b.outgoing,
		})
	}
	return groups
}

func (e *Engine) loadSnapshot(ctx context.Context) (stateSnapshot, error) {
	var snap stateSnapshot
	err := e.store.WithReadTx(ctx, func(r storage.SnapshotReader) error {
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
