package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
// storage.ErrNotFound. Statefulness is NOT a link constraint — volumes are
// orthogonal to identity, so any service (stateful or not) is a valid target.
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

// ServiceSource is a deploy target's recorded source plus whether it is
// stateful. up reads this before building so it knows the repo to build (or the
// prebuilt image to use). Missing fields mean the service has no source on file.
type ServiceSource struct {
	Stateful bool
	Source   Source
}

// ServiceSource looks up the source `add` recorded for the (project,
// environment, service) target. An unknown target surfaces as
// storage.ErrNotFound, so up can tell the user to `add` the service first.
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

// DeployInput is one service's resolved deploy commit: the target it lands on,
// the concrete image to run, sizing/health/restart knobs, and the replica count
// for its region. The image is already resolved (built or taken as-is) by the
// caller — this layer just commits it.
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

// Deploy commits a new current deployment for an existing service in one
// transaction: it resolves the service binding, bumps the version, supersedes
// the prior current commit, inserts the new one, and sets its region's replica
// count. The service must already exist (via `add`); an unknown target surfaces
// as storage.ErrNotFound.
func (s *Service) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	var res DeployResult
	err := s.store.WithTx(ctx, func(st Store) error {
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

// ParseVersion turns a `--to` argument into a deployment version. It accepts
// "" (→ 0, meaning "the version before current"), "vN", or "N". A non-positive
// or malformed value is an error. Lives here, not in the CLI, so the version
// grammar is owned alongside the rollback logic it feeds.
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

// Rollback re-points is_current to an EXISTING earlier deployment in one
// transaction — no rebuild, no new row. The target's image_ref/env/sizing are
// reused verbatim (it never consults config.toml); the engine then converges to
// it like any current deployment. Errors: an unknown target or no current
// deployment surface as storage.ErrNotFound; rolling back to the current version
// or with no earlier version available is rejected.
func (s *Service) Rollback(ctx context.Context, in RollbackInput) (RollbackResult, error) {
	var res RollbackResult
	err := s.store.WithTx(ctx, func(st Store) error {
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
