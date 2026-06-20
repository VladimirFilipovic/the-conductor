package storage

import (
	"context"
	"encoding/json"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// Store is the full control-plane persistence contract. It lives in the package
// that implements it (alongside PostgresClient) so that consumers — project,
// engine, status — depend on storage, never the other way round. The concrete
// *PostgresClient satisfies it; tests substitute fakes.
type Store interface {
	CreateProject(ctx context.Context, name string) (db.Project, error)
	GetProject(ctx context.Context, name string) (db.Project, error)
	CreateEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error)
	GetEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error)
	CreateService(ctx context.Context, projectName string, name string, stateful bool) (db.Service, error)
	GetService(ctx context.Context, projectName string, name string) (db.Service, error)
	ListServicesByEnvironment(ctx context.Context, projectName string, environment string) ([]db.Service, error)
	AddServiceToEnvironment(ctx context.Context, environmentID, serviceID uuid.UUID, source json.RawMessage) (db.EnvironmentService, error)

	// Environment listing/cloning backing `conductor environment list/create`.
	ListEnvironments(ctx context.Context, projectName string) ([]db.Environment, error)
	CloneEnvironmentServices(ctx context.Context, srcEnvironmentID, dstEnvironmentID uuid.UUID) (int64, error)

	// Deployment commit path backing `conductor up`.
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

	// Volume management backing `conductor volume`.
	CreateVolume(ctx context.Context, serviceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error)
	ListVolumesByService(ctx context.Context, projectName, service string) ([]db.Volume, error)
	UpdateVolumeSize(ctx context.Context, serviceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error)
	DeleteVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error)
}

// TxStore is a Store whose operations can be grouped into a transaction. The
// callback receives a tx-scoped Store; returning an error rolls back, returning
// nil commits. The callback's Store must not be retained after fn returns, and
// nesting WithTx is not supported.
type TxStore interface {
	Store
	WithTx(ctx context.Context, fn func(Store) error) error
}

// Compile-time proof that the concrete client satisfies the contract it now owns.
var _ TxStore = (*PostgresClient)(nil)
