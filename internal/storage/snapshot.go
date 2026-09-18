package storage

import (
	"context"

	"conductor/internal/storage/db"
)

// snapshotQuerier is the whole-fleet read slice of Querier: the state a
// reconcile pass diffs, plus the host roster the AgentAPI serves.
type snapshotQuerier interface {
	// SnapshotDesired returns one row per (current deployment, region): the
	// replica target plus the spec needed to mint a replica.
	SnapshotDesired(ctx context.Context) ([]db.SnapshotDesiredRow, error)
	// ListActiveReplicas returns the live fleet for services with a current
	// deployment — the observed half the diff compares against SnapshotDesired.
	// Includes replicas still under a superseded deployment (an in-flight
	// rollout); IsCurrent splits the new revision from the outgoing one.
	ListActiveReplicas(ctx context.Context) ([]db.ListActiveReplicasRow, error)
	// ListHealthyHosts returns every heartbeating host across all regions,
	// operator status included; the placer filters on status for free
	// placement and buckets by region for bin-packing.
	ListHealthyHosts(ctx context.Context) ([]db.Host, error)
	// ListAgentHosts returns every host an agent may attach as, regardless of
	// health or status — a cordoned or down host still has a session to serve.
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

func (q querier) ListHealthyHosts(ctx context.Context) ([]db.Host, error) {
	return q.queries.ListHealthyHosts(ctx)
}

func (q querier) ListAgentHosts(ctx context.Context) ([]db.Host, error) {
	return q.queries.ListAgentHosts(ctx)
}

func (q querier) ListActiveVolumes(ctx context.Context) ([]db.Volume, error) {
	return q.queries.ListActiveVolumes(ctx)
}
