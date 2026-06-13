package project

import (
	"context"
	"encoding/json"
	"fmt"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type Store interface {
	CreateProject(ctx context.Context, name string) (db.Project, error)
	CreateEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error)
	CreateService(ctx context.Context, projectName string, name string, stateful bool) (db.Service, error)
	AddServiceToEnvironment(ctx context.Context, environmentID, serviceID uuid.UUID, source json.RawMessage) (db.EnvironmentService, error)
}

// TxStore is a Store whose operations can be grouped into transaction .
// The callback receives a tx-scoped Store; returning an error rolls back,
// returning nil commits. The callback's Store must not be retained after fn
// returns, and nesting WithTx is not supported.
type TxStore interface {
	Store
	WithTx(ctx context.Context, fn func(Store) error) error
}

type Service struct {
	store TxStore
}

func NewService(store TxStore) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, name, env string) (db.Project, error) {
	var p db.Project
	err := s.store.WithTx(ctx, func(st Store) error {
		var err error
		if p, err = st.CreateProject(ctx, name); err != nil {
			return fmt.Errorf("create project %q: %w", name, err)
		}
		if _, err = st.CreateEnvironment(ctx, name, env); err != nil {
			return fmt.Errorf("create environment %q: %w", env, err)
		}
		return nil
	})
	return p, err
}

func (s *Service) AddService(ctx context.Context, projectName string, envID uuid.UUID, name string, stateful bool) (db.Service, error) {
	var svc db.Service
	err := s.store.WithTx(ctx, func(st Store) error {
		var err error
		if svc, err = st.CreateService(ctx, projectName, name, stateful); err != nil {
			return fmt.Errorf("create service %q: %w", name, err)
		}
		if _, err = st.AddServiceToEnvironment(ctx, envID, svc.ID, nil); err != nil {
			return fmt.Errorf("add service %q to project %q: %w", name, projectName, err)
		}
		return nil
	})
	return svc, err
}
