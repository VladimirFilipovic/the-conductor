package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// deploymentQuerier is the deployment slice of Querier: committing a new
// version, re-pointing current at an older one, and moving replica counts.
type deploymentQuerier interface {
	// Commit path backing `conductor up`.
	GetEnvironmentService(ctx context.Context, projectName, environment, service string) (db.GetEnvironmentServiceRow, error)
	NextDeploymentVersion(ctx context.Context, environmentServiceID uuid.UUID) (int32, error)
	SupersedeCurrentDeployments(ctx context.Context, environmentServiceID uuid.UUID) error
	CreateDeployment(ctx context.Context, arg db.CreateDeploymentParams) (db.Deployment, error)
	SetDeploymentRegion(ctx context.Context, deploymentID uuid.UUID, region string, replicas int32) error

	// Rollback path backing `conductor rollback`.
	GetCurrentDeployment(ctx context.Context, environmentServiceID uuid.UUID) (db.GetCurrentDeploymentRow, error)
	GetDeploymentByVersion(ctx context.Context, environmentServiceID uuid.UUID, version int32) (db.GetDeploymentByVersionRow, error)
	PreviousDeploymentVersion(ctx context.Context, environmentServiceID uuid.UUID, before int32) (int32, error)
	MarkCurrentRolledBack(ctx context.Context, environmentServiceID uuid.UUID) error
	SetDeploymentCurrent(ctx context.Context, deploymentID uuid.UUID) error

	// Replica-count mutations backing `conductor scale`/`down`.
	CurrentDeploymentID(ctx context.Context, projectName, environment, service string) (uuid.UUID, error)
	ZeroDeploymentRegions(ctx context.Context, deploymentID uuid.UUID) error
}

func (q querier) GetEnvironmentService(ctx context.Context, projectName, environment, service string) (db.GetEnvironmentServiceRow, error) {
	row, err := q.queries.GetEnvironmentService(ctx, db.GetEnvironmentServiceParams{
		ProjectName: projectName,
		Environment: environment,
		Service:     service,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return db.GetEnvironmentServiceRow{}, fmt.Errorf("service %q in %s/%s: %w", service, projectName, environment, ErrNotFound)
	}
	if err != nil {
		return db.GetEnvironmentServiceRow{}, err
	}
	return row, nil
}

func (q querier) NextDeploymentVersion(ctx context.Context, environmentServiceID uuid.UUID) (int32, error) {
	return q.queries.NextDeploymentVersion(ctx, environmentServiceID)
}

func (q querier) SupersedeCurrentDeployments(ctx context.Context, environmentServiceID uuid.UUID) error {
	return q.queries.SupersedeCurrentDeployments(ctx, environmentServiceID)
}

func (q querier) CreateDeployment(ctx context.Context, arg db.CreateDeploymentParams) (db.Deployment, error) {
	return q.queries.CreateDeployment(ctx, arg)
}

func (q querier) SetDeploymentRegion(ctx context.Context, deploymentID uuid.UUID, region string, replicas int32) error {
	return q.queries.SetDeploymentRegion(ctx, db.SetDeploymentRegionParams{
		DeploymentID: deploymentID,
		Region:       region,
		Replicas:     replicas,
	})
}

func (q querier) GetCurrentDeployment(ctx context.Context, environmentServiceID uuid.UUID) (db.GetCurrentDeploymentRow, error) {
	row, err := q.queries.GetCurrentDeployment(ctx, environmentServiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.GetCurrentDeploymentRow{}, fmt.Errorf("no current deployment (run `conductor up` first): %w", ErrNotFound)
	}
	if err != nil {
		return db.GetCurrentDeploymentRow{}, err
	}
	return row, nil
}

func (q querier) GetDeploymentByVersion(ctx context.Context, environmentServiceID uuid.UUID, version int32) (db.GetDeploymentByVersionRow, error) {
	row, err := q.queries.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
		EnvironmentServiceID: environmentServiceID,
		Version:              version,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return db.GetDeploymentByVersionRow{}, ErrNotFound
	}
	if err != nil {
		return db.GetDeploymentByVersionRow{}, err
	}
	return row, nil
}

func (q querier) PreviousDeploymentVersion(ctx context.Context, environmentServiceID uuid.UUID, before int32) (int32, error) {
	v, err := q.queries.PreviousDeploymentVersion(ctx, db.PreviousDeploymentVersionParams{
		EnvironmentServiceID: environmentServiceID,
		Before:               before,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}

func (q querier) MarkCurrentRolledBack(ctx context.Context, environmentServiceID uuid.UUID) error {
	return q.queries.MarkCurrentRolledBack(ctx, environmentServiceID)
}

func (q querier) SetDeploymentCurrent(ctx context.Context, deploymentID uuid.UUID) error {
	return q.queries.SetDeploymentCurrent(ctx, deploymentID)
}

func (q querier) CurrentDeploymentID(ctx context.Context, projectName, environment, service string) (uuid.UUID, error) {
	id, err := q.queries.CurrentDeploymentID(ctx, db.CurrentDeploymentIDParams{
		ProjectName: projectName,
		Environment: environment,
		Service:     service,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.UUID{}, fmt.Errorf("service %q in %s/%s has no active deployment: %w", service, projectName, environment, ErrNotFound)
	}
	if err != nil {
		return uuid.UUID{}, err
	}
	return id, nil
}

func (q querier) ZeroDeploymentRegions(ctx context.Context, deploymentID uuid.UUID) error {
	return q.queries.ZeroDeploymentRegions(ctx, deploymentID)
}
