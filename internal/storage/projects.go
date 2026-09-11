package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// projectQuerier is the project/environment/service slice of Querier: the
// naming tree everything else hangs off.
type projectQuerier interface {
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

	// Observed-vs-desired read backing `conductor status`.
	ProjectStatus(ctx context.Context, projectName, environment, service string) ([]db.ProjectStatusRow, error)
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

func (q querier) ListEnvironments(ctx context.Context, projectName string) ([]db.Environment, error) {
	return q.queries.ListEnvironments(ctx, projectName)
}

func (q querier) CloneEnvironmentServices(ctx context.Context, srcEnvironmentID, dstEnvironmentID uuid.UUID) (int64, error) {
	return q.queries.CloneEnvironmentServices(ctx, db.CloneEnvironmentServicesParams{
		DstEnvironmentID: dstEnvironmentID,
		SrcEnvironmentID: srcEnvironmentID,
	})
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
