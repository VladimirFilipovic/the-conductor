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

var ErrNotFound = errors.New("storage: not found")

var ErrExists = errors.New("storage: already exists")

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
		pool.Close()
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
	defer tx.Rollback()

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
