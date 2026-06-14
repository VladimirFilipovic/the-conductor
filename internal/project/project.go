package project

import (
	"context"
	"encoding/json"
	"fmt"

	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
)

type Store interface {
	CreateProject(ctx context.Context, name string) (db.Project, error)
	GetProject(ctx context.Context, name string) (db.Project, error)
	CreateEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error)
	GetEnvironment(ctx context.Context, projectName string, name string) (db.Environment, error)
	CreateService(ctx context.Context, projectName string, name string, stateful bool) (db.Service, error)
	GetService(ctx context.Context, projectName string, name string) (db.Service, error)
	ListServicesByEnvironment(ctx context.Context, projectName string, environment string) ([]db.Service, error)
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

func New(store TxStore) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, name, env string) (db.Project, error) {
	var p db.Project
	err := s.store.WithTx(ctx, func(st Store) error {
		var err error
		if p, err = st.CreateProject(ctx, name); err != nil {
			return err
		}
		if _, err = st.CreateEnvironment(ctx, name, env); err != nil {
			return err
		}
		return nil
	})
	return p, err
}

// Verify checks that every non-empty field of t names a row that can actually
// be linked, so callers (e.g. `link`) fail loudly instead of writing a pointer
// at a project/environment/service that isn't there. Project is always checked;
// environment and service are checked only when set. Missing rows surface as
// storage.ErrNotFound. A service that resolves to a database (stateful) is
// rejected: only stateless services are a meaningful link target.
func (s *Service) Verify(ctx context.Context, t target.Target) error {
	if _, err := s.store.GetProject(ctx, t.Project); err != nil {
		return err
	}
	if t.Environment != "" {
		if _, err := s.store.GetEnvironment(ctx, t.Project, t.Environment); err != nil {
			return err
		}
	}
	if t.Service != "" {
		svc, err := s.store.GetService(ctx, t.Project, t.Service)
		if err != nil {
			return err
		}
		if svc.Stateful {
			return fmt.Errorf("service %q is a database; only stateless services can be linked", t.Service)
		}
	}
	return nil
}

// ListServices returns the services bound to t's environment, ordered by name.
// The environment is verified first so an unknown project/environment surfaces
// as storage.ErrNotFound rather than an empty list that looks like "no
// services".
func (s *Service) ListServices(ctx context.Context, t target.Target) ([]db.Service, error) {
	if _, err := s.store.GetEnvironment(ctx, t.Project, t.Environment); err != nil {
		return nil, err
	}
	return s.store.ListServicesByEnvironment(ctx, t.Project, t.Environment)
}

// Source is the per-environment "how to obtain the code" record stored in the
// environment_services.source jsonb. A code service carries a repo (or image);
// both empty means "build from the linked working tree at deploy time".
type Source struct {
	Repo  string `json:"repo,omitempty"`
	Image string `json:"image,omitempty"`
}

// AddServiceInput is the input for AddService. The embedded Target names the
// project/environment the service is created in and the service's own name.
type AddServiceInput struct {
	target.Target
	Stateful bool
	Source   Source
}

// AddService creates a service in the project and binds it to the named
// environment in one transaction. The environment is looked up by name inside
// the tx so the bind can't race a concurrent environment delete.
func (s *Service) AddService(ctx context.Context, in AddServiceInput) (db.Service, error) {
	source, err := json.Marshal(in.Source)
	if err != nil {
		return db.Service{}, err
	}

	var svc db.Service
	err = s.store.WithTx(ctx, func(st Store) error {
		env, err := st.GetEnvironment(ctx, in.Project, in.Environment)
		if err != nil {
			return err
		}
		if svc, err = st.CreateService(ctx, in.Project, in.Service, in.Stateful); err != nil {
			return err
		}
		if _, err = st.AddServiceToEnvironment(ctx, env.ID, svc.ID, source); err != nil {
			return err
		}
		return nil
	})
	return svc, err
}
