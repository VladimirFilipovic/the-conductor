package project

import (
	"context"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
)

// ListEnvironments returns the project's environments, ordered by name; the
// project is verified first so an unknown one errs instead of an empty list.
func (s *Service) ListEnvironments(ctx context.Context, projectName string) ([]db.Environment, error) {
	return listEnvironments(ctx, s.store, projectName)
}

func listEnvironments(ctx context.Context, st ProjectStore, projectName string) ([]db.Environment, error) {
	if _, err := st.GetProject(ctx, projectName); err != nil {
		return nil, err
	}
	return st.ListEnvironments(ctx, projectName)
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
	var res CreateEnvironmentResult
	err := s.store.WithTx(ctx, func(st storage.Querier) error {
		var err error
		res, err = createEnvironment(ctx, st, projectName, sourceEnv, name)
		return err
	})
	return res, err
}

func createEnvironment(ctx context.Context, st ProjectStore, projectName, sourceEnv, name string) (CreateEnvironmentResult, error) {
	res := CreateEnvironmentResult{SourceEnv: sourceEnv}
	env, err := st.CreateEnvironment(ctx, projectName, name)
	if err != nil {
		return CreateEnvironmentResult{}, err
	}
	res.Environment = env
	if sourceEnv == "" {
		return res, nil
	}
	src, err := st.GetEnvironment(ctx, projectName, sourceEnv)
	if err != nil {
		return CreateEnvironmentResult{}, err
	}
	if res.ServicesCloned, err = st.CloneEnvironmentServices(ctx, src.ID, env.ID); err != nil {
		return CreateEnvironmentResult{}, err
	}
	return res, nil
}
