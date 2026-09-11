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
// reads the HTTP API serves, host cordon/drain, and gateway-instance
// bookkeeping.
type controlPlaneQuerier interface {
	ListReplicas(ctx context.Context) ([]db.Replica, error)

	CordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	UncordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DrainHost(ctx context.Context, hostID uuid.UUID) (bool, error)

	RegisterGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error
	HeartbeatGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error
	DeregisterGatewayInstance(ctx context.Context, id uuid.UUID) error
	DeleteStaleGatewayInstances(ctx context.Context, heartbeatBefore time.Time) error
	OldestLiveGatewayStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error)

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
	n, err := q.queries.DrainHost(ctx, hostID)
	return n > 0, err
}

func (q querier) RegisterGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error {
	return q.queries.RegisterGatewayInstance(ctx, db.RegisterGatewayInstanceParams{ID: id, Now: now})
}

func (q querier) HeartbeatGatewayInstance(ctx context.Context, id uuid.UUID, now time.Time) error {
	return q.queries.HeartbeatGatewayInstance(ctx, db.HeartbeatGatewayInstanceParams{ID: id, Now: now})
}

func (q querier) DeregisterGatewayInstance(ctx context.Context, id uuid.UUID) error {
	return q.queries.DeregisterGatewayInstance(ctx, id)
}

func (q querier) DeleteStaleGatewayInstances(ctx context.Context, heartbeatBefore time.Time) error {
	return q.queries.DeleteStaleGatewayInstances(ctx, heartbeatBefore)
}

// OldestLiveGatewayStart reports when the longest-running live apiserver
// started; ok=false means no instance has heartbeated since heartbeatAfter.
func (q querier) OldestLiveGatewayStart(ctx context.Context, heartbeatAfter time.Time) (time.Time, bool, error) {
	t, err := q.queries.OldestLiveGatewayStart(ctx, heartbeatAfter)
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
