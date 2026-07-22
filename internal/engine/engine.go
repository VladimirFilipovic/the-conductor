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
	Reconcile(snap stateSnapshot) []Intent
}

type actuator interface {
	Apply(ctx context.Context, intents []Intent) error
}

// Engine drives the reconcile loop: it snapshots state, asks the Reconciler
// what should change, and hands the resulting intents to the Actuator to
// commit.
type Engine struct {
	store      SnapshotStore
	reconciler reconciler
	actuator   actuator
}

func New(store SnapshotStore, r reconciler, a actuator) *Engine {
	return &Engine{store: store, reconciler: r, actuator: a}
}

func (e *Engine) run(ctx context.Context) error {
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

func (e *Engine) reconcile(ctx context.Context) error {
	snap, err := e.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("engine: load snapshot: %w", err)
	}

	intents := e.reconciler.Reconcile(snap)

	if err := e.actuator.Apply(ctx, intents); err != nil {
		return fmt.Errorf("engine: apply intents: %w", err)
	}

	logPass(snap, intents)
	return nil
}

// logPass is the one-line-per-tick summary. Skips are holds, not work, so a
// pass that only holds stays at Debug — Info fires only when the pass actually
// changed something, keeping a steady fleet's log silent.
func logPass(snap stateSnapshot, intents []Intent) {
	work := make(map[IntentKind]int)
	holds := 0
	for _, it := range intents {
		if it.Kind == IntentSkip {
			holds++
			continue
		}
		work[it.Kind]++
	}
	switch {
	case len(work) > 0:
		slog.Info("engine -> pass applied", "intents", work, "holds", holds)
	case holds > 0:
		slog.Debug("engine -> pass holding", "holds", holds)
	}
	slog.Debug("engine -> snapshot",
		"desired", len(snap.desired), "replicas", len(snap.replicas),
		"hosts", len(snap.hosts), "volumes", len(snap.volumes))
}
