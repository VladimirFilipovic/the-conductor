package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// controlPlaneQuerier is the operator slice of Querier: the topology/roster
// reads the HTTP API serves, host cordon/drain, and apiserver-instance
// bookkeeping.
type controlPlaneQuerier interface {
	ListReplicas(ctx context.Context) ([]db.Replica, error)

	CordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	UncordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DrainHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	// RestartReplica thaws a frozen replica back into the re-placement path.
	// ErrNotFound when no such row, ErrConflict when it exists but is not failed.
	RestartReplica(ctx context.Context, replicaID uuid.UUID) error
	ListHostReplicaCounts(ctx context.Context) (map[uuid.UUID]int64, error)

	UpsertApiserverInstance(ctx context.Context, id uuid.UUID, startedAt, now time.Time) error
	DeregisterApiserverInstance(ctx context.Context, id uuid.UUID) error
	DeleteStaleApiserverInstances(ctx context.Context, heartbeatBefore time.Time) error
	OldestLiveApiserverStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error)

	ListProjectNames(ctx context.Context, project string) ([]string, error)
	ListRegionNames(ctx context.Context) ([]string, error)
	ListEnvironmentRows(ctx context.Context, project, environment string) ([]db.ListEnvironmentRowsRow, error)
	ListProjectServices(ctx context.Context, project, environment string) ([]db.ListProjectServicesRow, error)
	ListDeploymentReplicaIDs(ctx context.Context, deploymentID uuid.UUID) ([]uuid.UUID, error)

	TopologyServices(ctx context.Context, f TopologyFilter) ([]db.TopologyServicesRow, error)
	TopologyDesiredRegions(ctx context.Context, f TopologyFilter) ([]db.TopologyDesiredRegionsRow, error)
	TopologyReplicas(ctx context.Context, f TopologyFilter) ([]db.TopologyReplicasRow, error)
	TopologyHosts(ctx context.Context, region string) ([]db.Host, error)
	TopologyServed(ctx context.Context, f TopologyFilter) ([]db.TopologyServedRow, error)
}

func (q querier) ListReplicas(ctx context.Context) ([]db.Replica, error) {
	return q.queries.ListReplicas(ctx)
}

// Operator host transitions. rows = 0 means the host was not in a state the
// transition applies to (or does not exist) — the caller reports it as a
// conflict, not a success.
func (q querier) CordonHost(ctx context.Context, hostID uuid.UUID) (bool, error) {
	n, err := q.queries.CordonHost(ctx, hostID)
	return n > 0, err
}

func (q querier) UncordonHost(ctx context.Context, hostID uuid.UUID) (bool, error) {
	n, err := q.queries.UncordonHost(ctx, hostID)
	return n > 0, err
}

func (q querier) DrainHost(ctx context.Context, hostID uuid.UUID) (bool, error) {
	n, err := q.queries.DrainHost(ctx, db.DrainHostParams{
		HostID: hostID,
		Now:    sql.NullTime{Time: time.Now(), Valid: true},
	})
	return n > 0, err
}

func (q querier) RestartReplica(ctx context.Context, replicaID uuid.UUID) error {
	row, err := q.queries.RestartReplica(ctx, replicaID)
	if err != nil {
		return err
	}
	switch {
	case row.Restarted:
		return nil
	case !row.Found:
		return ErrNotFound
	default:
		return ErrConflict
	}
}

// ListHostReplicaCounts is live (non-terminal) replicas per host; hosts with
// none are absent from the map.
func (q querier) ListHostReplicaCounts(ctx context.Context) (map[uuid.UUID]int64, error) {
	rows, err := q.queries.ListHostReplicaCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int64, len(rows))
	for _, r := range rows {
		out[r.HostID.UUID] = r.Replicas
	}
	return out, nil
}

func (q querier) UpsertApiserverInstance(ctx context.Context, id uuid.UUID, startedAt, now time.Time) error {
	return q.queries.UpsertApiserverInstance(ctx, db.UpsertApiserverInstanceParams{ID: id, StartedAt: startedAt, Now: now})
}

func (q querier) DeregisterApiserverInstance(ctx context.Context, id uuid.UUID) error {
	return q.queries.DeregisterApiserverInstance(ctx, id)
}

func (q querier) DeleteStaleApiserverInstances(ctx context.Context, heartbeatBefore time.Time) error {
	return q.queries.DeleteStaleApiserverInstances(ctx, heartbeatBefore)
}

// OldestLiveApiserverStart reports when the longest-running live apiserver
// started; ok=false means no instance has heartbeated since heartbeatAfter.
func (q querier) OldestLiveApiserverStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error) {
	t, err := q.queries.OldestLiveApiserverStart(ctx, heartbeatAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return t, err == nil, err
}

// TopologyFilter narrows a topology read. An empty field means "no filter", so
// the same queries serve the whole-fleet view and any narrowed one.
type TopologyFilter struct {
	Project     string
	Environment string
	Region      string
}

func (q querier) ListProjectNames(ctx context.Context, project string) ([]string, error) {
	return q.queries.ListProjectNames(ctx, project)
}

func (q querier) ListRegionNames(ctx context.Context) ([]string, error) {
	return q.queries.ListRegionNames(ctx)
}

func (q querier) ListEnvironmentRows(ctx context.Context, project, environment string) ([]db.ListEnvironmentRowsRow, error) {
	return q.queries.ListEnvironmentRows(ctx, db.ListEnvironmentRowsParams{Project: project, Environment: environment})
}

func (q querier) TopologyServices(ctx context.Context, f TopologyFilter) ([]db.TopologyServicesRow, error) {
	return q.queries.TopologyServices(ctx, db.TopologyServicesParams{Project: f.Project, Environment: f.Environment})
}

func (q querier) TopologyDesiredRegions(ctx context.Context, f TopologyFilter) ([]db.TopologyDesiredRegionsRow, error) {
	return q.queries.TopologyDesiredRegions(ctx, db.TopologyDesiredRegionsParams{
		Project: f.Project, Environment: f.Environment, Region: f.Region,
	})
}

func (q querier) TopologyReplicas(ctx context.Context, f TopologyFilter) ([]db.TopologyReplicasRow, error) {
	return q.queries.TopologyReplicas(ctx, db.TopologyReplicasParams{
		Project: f.Project, Environment: f.Environment, Region: f.Region,
	})
}

func (q querier) TopologyHosts(ctx context.Context, region string) ([]db.Host, error) {
	return q.queries.TopologyHosts(ctx, region)
}

func (q querier) TopologyServed(ctx context.Context, f TopologyFilter) ([]db.TopologyServedRow, error) {
	return q.queries.TopologyServed(ctx, db.TopologyServedParams{Project: f.Project, Environment: f.Environment})
}

func (q querier) ListProjectServices(ctx context.Context, project, environment string) ([]db.ListProjectServicesRow, error) {
	return q.queries.ListProjectServices(ctx, db.ListProjectServicesParams{Project: project, Environment: environment})
}

func (q querier) ListDeploymentReplicaIDs(ctx context.Context, deploymentID uuid.UUID) ([]uuid.UUID, error) {
	return q.queries.ListDeploymentReplicaIDs(ctx, deploymentID)
}
