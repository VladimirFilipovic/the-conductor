package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
)

// ErrInvalid marks a request the caller got wrong (a rule this layer enforces,
// like the single-instance cap on stateful services) as opposed to a missing
// row or a database fault, so an HTTP caller can answer 4xx instead of 500.
var ErrInvalid = errors.New("invalid request")

// Store is the persistence contract this package needs, composed of the three
// domain slices a single project workflow can span. It is declared here, on the
// consumer side: storage exports only the tx-scoped storage.Querier, and each
// helper below takes the one slice it actually touches.
type Store interface {
	ProjectStore
	DeploymentStore
	VolumeStore
}

// TxStore is a Store whose operations can be grouped into a transaction. The
// callback receives a tx-scoped storage.Querier; returning an error rolls back,
// returning nil commits. Bodies narrow it to Store (or a single slice) at the
// point of use rather than calling through the wide type.
type TxStore interface {
	Store
	WithTx(ctx context.Context, fn func(storage.Querier) error) error
}

var (
	_ Store   = storage.Querier(nil)
	_ TxStore = (*storage.PostgresClient)(nil)
)

// ProjectStore is the project/environment/service slice: the naming tree
// everything else hangs off.
type ProjectStore interface {
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
}

// DeploymentStore is the deployment slice: committing a new version,
// re-pointing current at an older one, and moving replica counts.
type DeploymentStore interface {
	// Commit path backing `conductor up`.
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
}

type Service struct {
	store TxStore
}

func New(store TxStore) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, name, env string) (db.Project, error) {
	var p db.Project
	err := s.store.WithTx(ctx, func(st storage.Querier) error {
		var err error
		p, err = createProject(ctx, st, name, env)
		return err
	})
	return p, err
}

// createProject seeds the project with its first environment; a project with no
// environment has nothing to deploy into, so the two are committed together.
func createProject(ctx context.Context, st ProjectStore, name, env string) (db.Project, error) {
	p, err := st.CreateProject(ctx, name)
	if err != nil {
		return db.Project{}, err
	}
	if _, err := st.CreateEnvironment(ctx, name, env); err != nil {
		return db.Project{}, err
	}
	return p, nil
}

// Verify checks that every non-empty field of t names an existing row, so
// callers (e.g. `link`) fail loudly instead of writing a dangling pointer.
// Missing rows surface as storage.ErrNotFound. Statefulness is NOT checked —
// volumes are orthogonal to identity, so any service is a valid target.
func (s *Service) Verify(ctx context.Context, t target.Target) error {
	return verifyTarget(ctx, s.store, t)
}

func verifyTarget(ctx context.Context, st ProjectStore, t target.Target) error {
	if _, err := st.GetProject(ctx, t.Project); err != nil {
		return err
	}
	if t.Environment != "" {
		if _, err := st.GetEnvironment(ctx, t.Project, t.Environment); err != nil {
			return err
		}
	}
	if t.Service != "" {
		if _, err := st.GetService(ctx, t.Project, t.Service); err != nil {
			return err
		}
	}
	return nil
}

// ListServices returns the environment's services, ordered by name. The
// environment is verified first so an unknown one surfaces as
// storage.ErrNotFound rather than an empty list that looks like "no services".
func (s *Service) ListServices(ctx context.Context, t target.Target) ([]db.Service, error) {
	return listServices(ctx, s.store, t)
}

func listServices(ctx context.Context, st ProjectStore, t target.Target) ([]db.Service, error) {
	if _, err := st.GetEnvironment(ctx, t.Project, t.Environment); err != nil {
		return nil, err
	}
	return st.ListServicesByEnvironment(ctx, t.Project, t.Environment)
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
	err = s.store.WithTx(ctx, func(st storage.Querier) error {
		svc, err = addService(ctx, st, in, source)
		return err
	})
	return svc, err
}

func addService(ctx context.Context, st ProjectStore, in AddServiceInput, source json.RawMessage) (db.Service, error) {
	env, err := st.GetEnvironment(ctx, in.Project, in.Environment)
	if err != nil {
		return db.Service{}, err
	}
	svc, err := st.CreateService(ctx, in.Project, in.Service, in.Stateful)
	if err != nil {
		return db.Service{}, err
	}
	if _, err := st.AddServiceToEnvironment(ctx, env.ID, svc.ID, source); err != nil {
		return db.Service{}, err
	}
	return svc, nil
}

// CreateService registers a service in the project without binding it to any
// environment. `conductor add` does both in one step because it always stands
// in an environment; the operator UI creates the service once and binds it per
// environment, so the two halves are separately callable.
func (s *Service) CreateService(ctx context.Context, projectName, name string, stateful bool) (db.Service, error) {
	return s.store.CreateService(ctx, projectName, name, stateful)
}

// BindServiceInput binds an existing service into an existing environment. Both
// are addressed by id because the caller picked them from a listing — resolving
// names again would only reopen the window for the row to have moved.
type BindServiceInput struct {
	EnvironmentID uuid.UUID
	ServiceID     uuid.UUID
	Source        Source
}

func (s *Service) BindService(ctx context.Context, in BindServiceInput) (db.EnvironmentService, error) {
	source, err := json.Marshal(in.Source)
	if err != nil {
		return db.EnvironmentService{}, err
	}
	return s.store.AddServiceToEnvironment(ctx, in.EnvironmentID, in.ServiceID, source)
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
	return serviceSource(ctx, s.store, t)
}

func serviceSource(ctx context.Context, st DeploymentStore, t target.Target) (ServiceSource, error) {
	row, err := st.GetEnvironmentService(ctx, t.Project, t.Environment, t.Service)
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
// Replicas is the per-region desired count; a region absent from the map is not
// targeted by this version at all.
type DeployInput struct {
	target.Target
	ImageRef         string
	CPUMillicores    int32
	MemBytes         int64
	Healthcheck      json.RawMessage
	DrainSeconds     int32
	RestartMax       int32
	ProgressDeadline int32
	Replicas         map[string]int32
	CommitMessage    string
	CreatedBy        string
}

// DeployResult reports the committed version and the replica topology it targets.
type DeployResult struct {
	Version  int32
	Replicas map[string]int32
}

// Deploy commits a new current deployment in one tx: bump version, supersede
// the prior current commit, insert the new one, set its region's replicas. The
// service must already exist (`add`); an unknown target ⇒ storage.ErrNotFound.
func (s *Service) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	var res DeployResult
	err := s.store.WithTx(ctx, func(st storage.Querier) error {
		var err error
		res, err = deploy(ctx, st, in)
		return err
	})
	return res, err
}

func deploy(ctx context.Context, st DeploymentStore, in DeployInput) (DeployResult, error) {
	es, err := st.GetEnvironmentService(ctx, in.Project, in.Environment, in.Service)
	if err != nil {
		return DeployResult{}, err
	}
	if total := totalReplicas(in.Replicas); es.Stateful && total > maxStatefulReplicas {
		return DeployResult{}, fmt.Errorf("%w: service %q is stateful and runs a single instance; total replicas must be <= %d, not %d", ErrInvalid, in.Service, maxStatefulReplicas, total)
	}
	version, err := st.NextDeploymentVersion(ctx, es.ID)
	if err != nil {
		return DeployResult{}, err
	}
	if err := st.SupersedeCurrentDeployments(ctx, es.ID); err != nil {
		return DeployResult{}, err
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
		ProgressDeadline:     in.ProgressDeadline,
		CommitMessage:        nullString(in.CommitMessage),
		CreatedBy:            nullString(in.CreatedBy),
	})
	if err != nil {
		return DeployResult{}, err
	}
	// Sorted so a multi-region commit writes its regions in a deterministic
	// order — two concurrent deploys can't deadlock on interleaved row locks.
	for _, region := range sortedRegions(in.Replicas) {
		if err := st.SetDeploymentRegion(ctx, dep.ID, region, in.Replicas[region]); err != nil {
			return DeployResult{}, err
		}
	}
	return DeployResult{Version: version, Replicas: in.Replicas}, nil
}

func totalReplicas(replicas map[string]int32) int32 {
	var total int32
	for _, count := range replicas {
		total += count
	}
	return total
}

func sortedRegions(replicas map[string]int32) []string {
	regions := make([]string, 0, len(replicas))
	for region := range replicas {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	return regions
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
	err := s.store.WithTx(ctx, func(st storage.Querier) error {
		var err error
		res, err = rollback(ctx, st, in)
		return err
	})
	return res, err
}

func rollback(ctx context.Context, st DeploymentStore, in RollbackInput) (RollbackResult, error) {
	es, err := st.GetEnvironmentService(ctx, in.Project, in.Environment, in.Service)
	if err != nil {
		return RollbackResult{}, err
	}
	current, err := st.GetCurrentDeployment(ctx, es.ID)
	if err != nil {
		return RollbackResult{}, err
	}

	target := in.ToVersion
	if target == 0 {
		prev, err := st.PreviousDeploymentVersion(ctx, es.ID, current.Version)
		if err != nil {
			return RollbackResult{}, fmt.Errorf("no earlier version to roll back to (current is v%d)", current.Version)
		}
		target = prev
	}
	if target == current.Version {
		return RollbackResult{}, fmt.Errorf("already at v%d", target)
	}
	dep, err := st.GetDeploymentByVersion(ctx, es.ID, target)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("no such version v%d", target)
	}

	if err := st.MarkCurrentRolledBack(ctx, es.ID); err != nil {
		return RollbackResult{}, err
	}
	if err := st.SetDeploymentCurrent(ctx, dep.ID); err != nil {
		return RollbackResult{}, err
	}
	return RollbackResult{From: current.Version, To: dep.Version}, nil
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
