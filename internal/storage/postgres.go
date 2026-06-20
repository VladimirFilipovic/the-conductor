package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"conductor/internal/project"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrNotFound = errors.New("not found")

var ErrExists = errors.New("already exists")

// uniqueViolation reports whether err is a Postgres unique-constraint failure
// (SQLSTATE 23505), letting Insert paths map a duplicate to ErrExists without a
// separate existence probe racing the write.
func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// fkViolation reports whether err is a Postgres foreign-key failure (SQLSTATE
// 23503), i.e. a referenced parent row does not exist, so Insert paths can map
// a missing parent to ErrNotFound instead of leaking a driver error.
func fkViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

type querier struct {
	queries *db.Queries
}

// PostgresClient is a project.TxStore backed by a database/sql pool over the
// pgx driver. SQL lives in db/queries and is compiled to type-safe Go by sqlc
// (the db package); this type only marshals values and maps the generated
// results onto the package's sentinel errors.
type PostgresClient struct {
	querier
	pool *sql.DB
}

// NewPostgresClient opens a pool to dsn and verifies the connection. The schema
// is expected to already exist — run `make migrate` first. dsn is a libpq/pgx
// connection string or URL, e.g. "postgres://user:pass@host:5432/conductor".
func NewPostgresClient(ctx context.Context, dsn string) (*PostgresClient, error) {
	// sql.Open is lazy and never errors on a bad host; Ping forces a real
	// connection so construction fails fast on an unreachable database.
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("storage: connect: %w", err)
	}
	return &PostgresClient{querier: querier{queries: db.New(pool)}, pool: pool}, nil
}

// Close releases the underlying connection pool.
func (c *PostgresClient) Close() error { return c.pool.Close() }

// WithTx runs fn against a tx-scoped Store and commits if it returns nil. Any
// error from fn (or commit) rolls the whole unit back, so multi-step workflows
// in the project package are all-or-nothing.
func (c *PostgresClient) WithTx(ctx context.Context, fn func(project.Store) error) error {
	tx, err := c.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	// No-op after a successful Commit; guarantees rollback on error or panic.
	defer func() { _ = tx.Rollback() }()

	if err := fn(querier{queries: c.queries.WithTx(tx)}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

func (q querier) CreateProject(ctx context.Context, name string) (db.Project, error) {
	p, err := q.queries.CreateProject(ctx, name)
	if uniqueViolation(err) {
		return db.Project{}, fmt.Errorf("project %q: %w", name, ErrExists)
	}
	if err != nil {
		return db.Project{}, err
	}
	return p, nil
}

func (q querier) GetProject(ctx context.Context, name string) (db.Project, error) {
	p, err := q.queries.GetProject(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Project{}, fmt.Errorf("project %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return db.Project{}, err
	}
	return p, nil
}

func (q querier) CreateEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error) {
	e, err := q.queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectName: projectName, Name: name})
	if uniqueViolation(err) {
		return db.Environment{}, fmt.Errorf("environment %q in project %q: %w", name, projectName, ErrExists)
	}
	if fkViolation(err) {
		return db.Environment{}, fmt.Errorf("project %q: %w", projectName, ErrNotFound)
	}
	if err != nil {
		return db.Environment{}, err
	}
	return e, nil
}

func (q querier) GetEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error) {
	e, err := q.queries.GetEnvironment(ctx, db.GetEnvironmentParams{ProjectName: projectName, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Environment{}, fmt.Errorf("environment %q in project %q: %w", name, projectName, ErrNotFound)
	}
	if err != nil {
		return db.Environment{}, err
	}
	return e, nil
}

func (q querier) CreateService(ctx context.Context, projectName string, name string, stateful bool) (db.Service, error) {
	s, err := q.queries.CreateService(ctx, db.CreateServiceParams{ProjectName: projectName, Name: name, Stateful: stateful})
	if uniqueViolation(err) {
		return db.Service{}, fmt.Errorf("service %q in project %q: %w", name, projectName, ErrExists)
	}
	if fkViolation(err) {
		return db.Service{}, fmt.Errorf("project %q: %w", projectName, ErrNotFound)
	}
	if err != nil {
		return db.Service{}, err
	}
	return s, nil
}

func (q querier) GetService(ctx context.Context, projectName string, name string) (db.Service, error) {
	s, err := q.queries.GetService(ctx, db.GetServiceParams{ProjectName: projectName, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Service{}, fmt.Errorf("service %q in project %q: %w", name, projectName, ErrNotFound)
	}
	if err != nil {
		return db.Service{}, err
	}
	return s, nil
}

func (q querier) ListServicesByEnvironment(ctx context.Context, projectName string, environment string) ([]db.Service, error) {
	return q.queries.ListServicesByEnvironment(ctx, db.ListServicesByEnvironmentParams{ProjectName: projectName, Name: environment})
}

func (q querier) ProjectStatus(ctx context.Context, projectName, environment, service string) ([]db.ProjectStatusRow, error) {
	return q.queries.ProjectStatus(ctx, db.ProjectStatusParams{
		ProjectName: projectName,
		Environment: environment,
		Service:     service,
	})
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

func (q querier) ListEnvironments(ctx context.Context, projectName string) ([]db.Environment, error) {
	return q.queries.ListEnvironments(ctx, projectName)
}

func (q querier) CloneEnvironmentServices(ctx context.Context, srcEnvironmentID, dstEnvironmentID uuid.UUID) (int64, error) {
	return q.queries.CloneEnvironmentServices(ctx, db.CloneEnvironmentServicesParams{
		DstEnvironmentID: dstEnvironmentID,
		SrcEnvironmentID: srcEnvironmentID,
	})
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

func (q querier) CreateVolume(ctx context.Context, serviceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.CreateVolume(ctx, db.CreateVolumeParams{
		ServiceID:        serviceID,
		Name:             name,
		Region:           region,
		MountPath:        mountPath,
		DesiredSizeBytes: sizeBytes,
	})
	if uniqueViolation(err) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrExists)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) ListVolumesByService(ctx context.Context, projectName, service string) ([]db.Volume, error) {
	return q.queries.ListVolumesByService(ctx, db.ListVolumesByServiceParams{ProjectName: projectName, Name: service})
}

func (q querier) UpdateVolumeSize(ctx context.Context, serviceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.UpdateVolumeSize(ctx, db.UpdateVolumeSizeParams{
		DesiredSizeBytes: sizeBytes,
		ServiceID:        serviceID,
		MountPath:        mountPath,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) DeleteVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error) {
	v, err := q.queries.DeleteVolume(ctx, db.DeleteVolumeParams{ServiceID: serviceID, MountPath: mountPath})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	// A replica still pins this volume (replicas.volume_id FK); `down` the service
	// first so the reconcile loop reaps the replicas, then the disk can go.
	if fkViolation(err) {
		return db.Volume{}, fmt.Errorf("volume at %q is still attached to a replica: scale the service down first", mountPath)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) AddServiceToEnvironment(ctx context.Context, environmentID, serviceID uuid.UUID, source json.RawMessage) (db.EnvironmentService, error) {
	if source == nil {
		source = json.RawMessage("{}")
	}
	es, err := q.queries.AddServiceToEnvironment(ctx, db.AddServiceToEnvironmentParams{
		EnvironmentID: environmentID,
		ServiceID:     serviceID,
		Source:        source,
	})
	if uniqueViolation(err) {
		return db.EnvironmentService{}, fmt.Errorf("service %s in environment %s: %w", serviceID, environmentID, ErrExists)
	}
	if fkViolation(err) {
		return db.EnvironmentService{}, fmt.Errorf("environment %s or service %s: %w", environmentID, serviceID, ErrNotFound)
	}
	if err != nil {
		return db.EnvironmentService{}, err
	}
	return es, nil
}
