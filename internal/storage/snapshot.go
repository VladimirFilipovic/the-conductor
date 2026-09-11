package storage

import (
	"context"

	"conductor/internal/storage/db"
)

// snapshotQuerier is the whole-fleet read slice of Querier: the state a
// reconcile pass diffs, plus the host roster the agent gateway serves.
type snapshotQuerier interface {
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
	// ListAgentHosts returns every host an agent may attach as, regardless of
	// schedulability — a cordoned or down host still has a session to serve.
	ListAgentHosts(ctx context.Context) ([]db.Host, error)
	// ListActiveVolumes returns the disks of services with a current deployment,
	// keyed by (service_id, region) against the stateful rows of SnapshotDesired.
	ListActiveVolumes(ctx context.Context) ([]db.Volume, error)
}

func (q querier) SnapshotDesired(ctx context.Context) ([]db.SnapshotDesiredRow, error) {
	return q.queries.SnapshotDesired(ctx)
}

func (q querier) ListActiveReplicas(ctx context.Context) ([]db.ListActiveReplicasRow, error) {
	return q.queries.ListActiveReplicas(ctx)
}

func (q querier) ListSchedulableHosts(ctx context.Context) ([]db.Host, error) {
	return q.queries.ListSchedulableHosts(ctx)
}

func (q querier) ListAgentHosts(ctx context.Context) ([]db.Host, error) {
	return q.queries.ListAgentHosts(ctx)
}

func (q querier) ListActiveVolumes(ctx context.Context) ([]db.Volume, error) {
	return q.queries.ListActiveVolumes(ctx)
}
