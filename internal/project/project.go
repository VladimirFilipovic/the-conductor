package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/internal/target"
)

type Service struct {
	store storage.TxStore
}

func New(store storage.TxStore) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, name, env string) (db.Project, error) {
	var p db.Project
	err := s.store.WithTx(ctx, func(st storage.Store) error {
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

// Verify checks that every non-empty field of t names an existing row, so
// callers (e.g. `link`) fail loudly instead of writing a dangling pointer.
// Missing rows surface as storage.ErrNotFound. Statefulness is NOT checked —
// volumes are orthogonal to identity, so any service is a valid target.
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
		if _, err := s.store.GetService(ctx, t.Project, t.Service); err != nil {
			return err
		}
	}
	return nil
}

// ListServices returns the environment's services, ordered by name. The
// environment is verified first so an unknown one surfaces as
// storage.ErrNotFound rather than an empty list that looks like "no services".
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

// AddServiceInput's embedded Target names the project/environment the service
// is created in and the service's own name.
type AddServiceInput struct {
	target.Target
	Stateful bool
	Source   Source
}

// AddService creates the service and binds it to the environment in one tx;
// the env is looked up inside the tx so the bind can't race an env delete.
func (s *Service) AddService(ctx context.Context, in AddServiceInput) (db.Service, error) {
	source, err := json.Marshal(in.Source)
	if err != nil {
		return db.Service{}, err
	}

	var svc db.Service
	err = s.store.WithTx(ctx, func(st storage.Store) error {
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

// ServiceSource is a deploy target's recorded source plus statefulness; up
// reads it before building. Missing fields mean no source on file.
type ServiceSource struct {
	Stateful bool
	Source   Source
}

// ServiceSource returns the source `add` recorded for the target. An unknown
// target surfaces as storage.ErrNotFound, so up can tell the user to `add` first.
func (s *Service) ServiceSource(ctx context.Context, t target.Target) (ServiceSource, error) {
	row, err := s.store.GetEnvironmentService(ctx, t.Project, t.Environment, t.Service)
	if err != nil {
		return ServiceSource{}, err
	}
	var src Source
	if err := json.Unmarshal(row.Source, &src); err != nil {
		return ServiceSource{}, fmt.Errorf("decode source for %q: %w", t.Service, err)
	}
	return ServiceSource{Stateful: row.Stateful, Source: src}, nil
}

// DeployInput is one service's resolved deploy commit. The image is already
// resolved (built or taken as-is) by the caller — this layer just commits it.
type DeployInput struct {
	target.Target
	ImageRef      string
	CPUMillicores int32
	MemBytes      int64
	Healthcheck   json.RawMessage
	DrainSeconds  int32
	RestartMax    int32
	Region        string
	NumReplicas   int32
	CommitMessage string
	CreatedBy     string
}

// DeployResult reports the committed version and the replica topology it targets.
type DeployResult struct {
	Version  int32
	Region   string
	Replicas int32
}

// Deploy commits a new current deployment in one tx: bump version, supersede
// the prior current commit, insert the new one, set its region's replicas. The
// service must already exist (`add`); an unknown target ⇒ storage.ErrNotFound.
func (s *Service) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	var res DeployResult
	err := s.store.WithTx(ctx, func(st storage.Store) error {
		es, err := st.GetEnvironmentService(ctx, in.Project, in.Environment, in.Service)
		if err != nil {
			return err
		}
		if es.Stateful && in.NumReplicas > maxStatefulReplicas {
			return fmt.Errorf("service %q is stateful and runs a single instance; set num_replicas to %d, not %d", in.Service, maxStatefulReplicas, in.NumReplicas)
		}
		version, err := st.NextDeploymentVersion(ctx, es.ID)
		if err != nil {
			return err
		}
		if err := st.SupersedeCurrentDeployments(ctx, es.ID); err != nil {
			return err
		}
		dep, err := st.CreateDeployment(ctx, db.CreateDeploymentParams{
			EnvironmentServiceID: es.ID,
			Version:              version,
			ImageRef:             in.ImageRef,
			CpuMillicores:        in.CPUMillicores,
			MemBytes:             in.MemBytes,
			Env:                  json.RawMessage("{}"),
			Healthcheck:          orEmptyJSON(in.Healthcheck),
			DrainSeconds:         in.DrainSeconds,
			RestartMax:           in.RestartMax,
			CommitMessage:        nullString(in.CommitMessage),
			CreatedBy:            nullString(in.CreatedBy),
		})
		if err != nil {
			return err
		}
		if err := st.SetDeploymentRegion(ctx, dep.ID, in.Region, in.NumReplicas); err != nil {
			return err
		}
		res = DeployResult{Version: version, Region: in.Region, Replicas: in.NumReplicas}
		return nil
	})
	return res, err
}

// ParseVersion parses a `--to` argument: "" (→ 0, "the version before
// current"), "vN", or "N". Lives here, not in the CLI, so the version grammar
// is owned alongside the rollback logic it feeds.
func ParseVersion(s string) (int32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "v"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a version like v2", s)
	}
	return int32(n), nil
}

// RollbackInput names the target to roll back. ToVersion == 0 means "the
// version before the current one".
type RollbackInput struct {
	target.Target
	ToVersion int32
}

// RollbackResult reports the version rolled away from and the one promoted.
type RollbackResult struct {
	From int32
	To   int32
}

// Rollback re-points is_current to an EXISTING earlier deployment in one tx —
// no rebuild, no new row; image_ref/env/sizing are reused verbatim (config.toml
// is never consulted). Unknown target or no current deployment ⇒
// storage.ErrNotFound; rolling back to the current version is rejected.
func (s *Service) Rollback(ctx context.Context, in RollbackInput) (RollbackResult, error) {
	var res RollbackResult
	err := s.store.WithTx(ctx, func(st storage.Store) error {
		es, err := st.GetEnvironmentService(ctx, in.Project, in.Environment, in.Service)
		if err != nil {
			return err
		}
		current, err := st.GetCurrentDeployment(ctx, es.ID)
		if err != nil {
			return err
		}

		target := in.ToVersion
		if target == 0 {
			prev, err := st.PreviousDeploymentVersion(ctx, es.ID, current.Version)
			if err != nil {
				return fmt.Errorf("no earlier version to roll back to (current is v%d)", current.Version)
			}
			target = prev
		}
		if target == current.Version {
			return fmt.Errorf("already at v%d", target)
		}
		dep, err := st.GetDeploymentByVersion(ctx, es.ID, target)
		if err != nil {
			return fmt.Errorf("no such version v%d", target)
		}

		if err := st.MarkCurrentRolledBack(ctx, es.ID); err != nil {
			return err
		}
		if err := st.SetDeploymentCurrent(ctx, dep.ID); err != nil {
			return err
		}
		res = RollbackResult{From: current.Version, To: dep.Version}
		return nil
	})
	return res, err
}

func orEmptyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
