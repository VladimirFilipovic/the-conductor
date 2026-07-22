package project

import (
	"context"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
)

// ListEnvironments returns the project's environments, ordered by name; the
// project is verified first so an unknown one errs instead of an empty list.
func (s *Service) ListEnvironments(ctx context.Context, projectName string) ([]db.Environment, error) {
	if _, err := s.store.GetProject(ctx, projectName); err != nil {
		return nil, err
	}
	return s.store.ListEnvironments(ctx, projectName)
}

// CreateEnvironmentResult reports the created environment and, when cloning from
// a source environment, how many service bindings were copied into it.
type CreateEnvironmentResult struct {
	Environment    db.Environment
	SourceEnv      string
	ServicesCloned int64
}

// CreateEnvironment creates the environment and, when sourceEnv is set, clones
// its service bindings in one tx so a half-populated environment is never
// visible. A blank sourceEnv creates an empty environment.
func (s *Service) CreateEnvironment(ctx context.Context, projectName, sourceEnv, name string) (CreateEnvironmentResult, error) {
	res := CreateEnvironmentResult{SourceEnv: sourceEnv}
	err := s.store.WithTx(ctx, func(st storage.Store) error {
		env, err := st.CreateEnvironment(ctx, projectName, name)
		if err != nil {
			return err
		}
		res.Environment = env
		if sourceEnv == "" {
			return nil
		}
		src, err := st.GetEnvironment(ctx, projectName, sourceEnv)
		if err != nil {
			return err
		}
		cloned, err := st.CloneEnvironmentServices(ctx, src.ID, env.ID)
		if err != nil {
			return err
		}
		res.ServicesCloned = cloned
		return nil
	})
	return res, err
}
