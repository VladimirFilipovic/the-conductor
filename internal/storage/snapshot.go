package storage

import (
	"context"

	"conductor/internal/storage/db"
)

// SnapshotReader is the read side of a reconcile pass: the desired-state and
// observed-state queries the Engine runs to compute its placement diff. It is
// handed to WithReadTx callbacks, where every read observes one REPEATABLE READ
// snapshot — so the desired count can't scale out from under the replica list
// between calls and tear the diff. Read-only; placement writes go through
// ReconcileTx.
type SnapshotReader interface {
	// SnapshotDesired returns one row per (current deployment, region): the
	// replica target plus the spec needed to mint a replica.
	SnapshotDesired(ctx context.Context) ([]db.SnapshotDesiredRow, error)
	// ListActiveReplicas returns the live fleet for services with a current
	// deployment — the observed half the diff compares against SnapshotDesired.
	// Includes replicas still under a superseded deployment (an in-flight
	// rollout); IsCurrent splits the new revision from the outgoing one.
	ListActiveReplicas(ctx context.Context) ([]db.ListActiveReplicasRow, error)
	// ListSchedulableHosts returns 'ready' hosts across all regions; the caller
	// buckets by region for bin-packing.
	ListSchedulableHosts(ctx context.Context) ([]db.Host, error)
	// ListActiveVolumes returns the disks of services with a current deployment,
	// keyed by (service_id, region) against the stateful rows of SnapshotDesired.
	ListActiveVolumes(ctx context.Context) ([]db.Volume, error)
}

// The tx-scoped querier handed to WithReadTx callbacks is exactly this view.
var _ SnapshotReader = querier{}

func (q querier) SnapshotDesired(ctx context.Context) ([]db.SnapshotDesiredRow, error) {
	return q.queries.SnapshotDesired(ctx)
}

func (q querier) ListActiveReplicas(ctx context.Context) ([]db.ListActiveReplicasRow, error) {
	return q.queries.ListActiveReplicas(ctx)
}

func (q querier) ListSchedulableHosts(ctx context.Context) ([]db.Host, error) {
	return q.queries.ListSchedulableHosts(ctx)
}

func (q querier) ListActiveVolumes(ctx context.Context) ([]db.Volume, error) {
	return q.queries.ListActiveVolumes(ctx)
}
