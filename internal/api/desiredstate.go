package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"conductor/internal/deployspec"
	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
)

// DesiredState is the desired-state half of the OperatorAPI: exactly the
// project-domain operations the operator UI drives, and nothing else.
// *project.Service satisfies it. Every rule these writes must obey (one-current
// deployment, the single-instance cap on stateful services, the transaction
// boundaries) lives behind this interface — the handlers only translate JSON.
type DesiredState interface {
	CreateProject(ctx context.Context, name, env string) (db.Project, error)
	CreateEnvironment(ctx context.Context, projectName, sourceEnv, name string) (project.CreateEnvironmentResult, error)
	CreateService(ctx context.Context, projectName, name string, stateful bool) (db.Service, error)
	BindService(ctx context.Context, in project.BindServiceInput) (db.EnvironmentService, error)
	LaunchService(ctx context.Context, in project.AddServiceInput, dep *project.DeployInput) (db.Service, *project.DeployResult, error)
	Deploy(ctx context.Context, in project.DeployInput) (project.DeployResult, error)
	Scale(ctx context.Context, in project.ScaleInput) (project.ScaleResult, error)
	// Volume grow and its one-shot take-back. Every guard — grow-only, one grow
	// in flight, revert only from resize_pending — is the project layer's; the
	// handlers translate JSON and map the sentinels.
	ResizeVolume(ctx context.Context, t target.Target, mountPath string, sizeBytes int64) (project.ResizeOutcome, error)
	RevertVolume(ctx context.Context, t target.Target, mountPath string) (db.Volume, error)
}

// --- Requests ---------------------------------------------------------------

type createProjectRequest struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

type createEnvironmentRequest struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	// SourceEnvironment clones that environment's service bindings into the new
	// one; empty creates it bare.
	SourceEnvironment string `json:"source_environment"`
}

type createServiceRequest struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Stateful bool   `json:"stateful"`
	// Environment binds the new service there with Source in the same tx, and
	// Deploy then commits its first version from Source.Image — the UI's "one
	// form to a running service". All three empty is the bare project-level
	// create, bound later per environment.
	Environment string         `json:"environment"`
	Source      project.Source `json:"source"`
	Deploy      *deploySpec    `json:"deploy"`
}

type bindServiceRequest struct {
	EnvironmentID string         `json:"environment_id"`
	ServiceID     string         `json:"service_id"`
	Source        project.Source `json:"source"`
}

// serviceTarget names the (project, environment, service) triple every
// deployment write addresses — the same identity the CLI takes from its folder
// link and -p/-e/-s flags.
type serviceTarget struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
}

type deployRequest struct {
	serviceTarget
	ImageRef string `json:"image_ref"`
	deploySpec
}

// deploySpec is a deployment's sizing and rollout knobs, shared by an explicit
// deploy and the first deploy a service launch carries.
type deploySpec struct {
	CPUMillicores    int32            `json:"cpu_millicores"`
	MemBytes         int64            `json:"mem_bytes"`
	DrainSeconds     int32            `json:"drain_seconds"`
	RestartMax       int32            `json:"restart_max"`
	ProgressDeadline int32            `json:"progress_deadline"`
	CommitMessage    string           `json:"commit_message"`
	CreatedBy        string           `json:"created_by"`
	Replicas         map[string]int32 `json:"replicas"`
}

type scaleRequest struct {
	serviceTarget
	Replicas map[string]int32 `json:"replicas"`
	// Optional new per-replica limits; a change commits a new version rather
	// than patching counts in place (see project.ScaleInput).
	CPUMillicores int32  `json:"cpu_millicores"`
	MemBytes      int64  `json:"mem_bytes"`
	CreatedBy     string `json:"created_by"`
}

// A volume is addressed the way the CLI addresses it: the service target plus
// the mount path, never the volume UUID.
type volumeResizeRequest struct {
	serviceTarget
	MountPath string `json:"mount_path"`
	SizeBytes int64  `json:"size_bytes"`
}

type volumeRevertRequest struct {
	serviceTarget
	MountPath string `json:"mount_path"`
}

// --- Responses --------------------------------------------------------------

type projectCreatedJSON struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

type environmentCreatedJSON struct {
	ID                uuid.UUID `json:"id"`
	Project           string    `json:"project"`
	Name              string    `json:"name"`
	SourceEnvironment string    `json:"source_environment"`
	ServicesCloned    int64     `json:"services_cloned"`
}

type serviceJSON struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Stateful bool      `json:"stateful"`
}

type servicesJSON struct {
	Services []serviceJSON `json:"services"`
}

type environmentServiceJSON struct {
	ID uuid.UUID `json:"id"`
}

// launchedJSON is the created service plus, when the launch carried a deploy,
// the version it committed; 0 means created (and maybe bound), not deployed.
type launchedJSON struct {
	serviceJSON
	Version int32 `json:"version,omitempty"`
}

// scaledJSON's Version is set only when new limits made the scale a resize.
type scaledJSON struct {
	OK      bool  `json:"ok"`
	Version int32 `json:"version,omitempty"`
}

type deployedJSON struct {
	Version  int32            `json:"version"`
	Replicas map[string]int32 `json:"replicas"`
}

// volumeResizedJSON relays the project layer's space advisory so the UI can
// say up front whether the grow starts next tick or parks as resize_pending.
type volumeResizedJSON struct {
	OK              bool  `json:"ok"`
	WaitingForSpace bool  `json:"waiting_for_space"`
	ShortfallBytes  int64 `json:"shortfall_bytes"`
}

type volumeRevertedJSON struct {
	OK               bool  `json:"ok"`
	DesiredSizeBytes int64 `json:"desired_size_bytes"`
}

// --- Handlers ---------------------------------------------------------------

// listServices backs the bind form: project-wide by default so a service can be
// bound into an environment it is not in yet; ?environment= narrows to the
// services already bound there.
func (o *OperatorAPI) listServices(w http.ResponseWriter, r *http.Request) {
	projectName := filterValue(r, "project")
	if projectName == "" {
		writeError(w, http.StatusBadRequest, errors.New("project is required"))
		return
	}
	rows, err := o.store.ListProjectServices(r.Context(), projectName, filterValue(r, "environment"))
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	out := make([]serviceJSON, len(rows))
	for i, row := range rows {
		out[i] = serviceJSON{ID: row.ID, Name: row.Name, Stateful: row.Stateful}
	}
	writeJSON(w, http.StatusOK, servicesJSON{Services: out})
}

func (o *OperatorAPI) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	// A project with no environment has nothing to deploy into, so it is born
	// with one — same rule `conductor init` follows.
	env := orDefault(req.Environment, link.DefaultEnvironment)
	if _, err := o.desired.CreateProject(r.Context(), req.Name, env); err != nil {
		writeDomainError(w, r, err)
		return
	}
	slog.Info("operatorapi -> project created", "project", req.Name, "environment", env)
	writeJSON(w, http.StatusCreated, projectCreatedJSON{Name: req.Name, Environment: env})
}

func (o *OperatorAPI) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req createEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Project == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("project and name are required"))
		return
	}
	res, err := o.desired.CreateEnvironment(r.Context(), req.Project, req.SourceEnvironment, req.Name)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	slog.Info("operatorapi -> environment created", "project", req.Project, "environment", req.Name)
	writeJSON(w, http.StatusCreated, environmentCreatedJSON{
		ID:                res.Environment.ID,
		Project:           req.Project,
		Name:              res.Environment.Name,
		SourceEnvironment: res.SourceEnv,
		ServicesCloned:    res.ServicesCloned,
	})
}

func (o *OperatorAPI) createService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Environment == "" {
		svc, err := o.desired.CreateService(r.Context(), req.Project, req.Name, req.Stateful)
		if err != nil {
			writeDomainError(w, r, err)
			return
		}
		slog.Info("operatorapi -> service created", "project", req.Project, "service", req.Name)
		writeJSON(w, http.StatusCreated, launchedJSON{serviceJSON: serviceJSON{ID: svc.ID, Name: svc.Name, Stateful: svc.Stateful}})
		return
	}

	t := target.Target{Project: req.Project, Environment: req.Environment, Service: req.Name}
	var dep *project.DeployInput
	if req.Deploy != nil {
		in := req.Deploy.input(t, req.Source.Image)
		dep = &in
	}
	svc, res, err := o.desired.LaunchService(r.Context(),
		project.AddServiceInput{Target: t, Stateful: req.Stateful, Source: req.Source}, dep)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	out := launchedJSON{serviceJSON: serviceJSON{ID: svc.ID, Name: svc.Name, Stateful: svc.Stateful}}
	if res != nil {
		out.Version = res.Version
	}
	slog.Info("operatorapi -> service launched", "target", t, "version", out.Version)
	writeJSON(w, http.StatusCreated, out)
}

func (o *OperatorAPI) bindService(w http.ResponseWriter, r *http.Request) {
	var req bindServiceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	environmentID, err := uuid.Parse(req.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad environment_id"))
		return
	}
	serviceID, err := uuid.Parse(req.ServiceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad service_id"))
		return
	}
	es, err := o.desired.BindService(r.Context(), project.BindServiceInput{
		EnvironmentID: environmentID, ServiceID: serviceID, Source: req.Source,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	slog.Info("operatorapi -> service bound", "environment", environmentID, "service", serviceID)
	writeJSON(w, http.StatusCreated, environmentServiceJSON{ID: es.ID})
}

func (o *OperatorAPI) deploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := o.desired.Deploy(r.Context(), req.input(req.target(), req.ImageRef))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	slog.Info("operatorapi -> deployed", "target", req.serviceTarget, "version", res.Version)
	writeJSON(w, http.StatusCreated, deployedJSON{Version: res.Version, Replicas: res.Replicas})
}

func (o *OperatorAPI) scale(w http.ResponseWriter, r *http.Request) {
	var req scaleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := o.desired.Scale(r.Context(), project.ScaleInput{
		Target: req.target(), Replicas: req.Replicas,
		CPUMillicores: req.CPUMillicores, MemBytes: req.MemBytes, CreatedBy: req.CreatedBy,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	slog.Info("operatorapi -> scaled", "target", req.serviceTarget, "replicas", req.Replicas, "resize_version", res.Version)
	writeJSON(w, http.StatusOK, scaledJSON{OK: true, Version: res.Version})
}

func (o *OperatorAPI) resizeVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeResizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := o.desired.ResizeVolume(r.Context(), req.target(), req.MountPath, req.SizeBytes)
	if err != nil {
		writeVolumeError(w, r, err)
		return
	}
	slog.Info("operatorapi -> volume resize requested", "target", req.serviceTarget,
		"mount", req.MountPath, "size_bytes", req.SizeBytes, "waiting_for_space", out.WaitingForSpace)
	writeJSON(w, http.StatusOK, volumeResizedJSON{
		OK: true, WaitingForSpace: out.WaitingForSpace, ShortfallBytes: out.ShortfallBytes,
	})
}

func (o *OperatorAPI) revertVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRevertRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	vol, err := o.desired.RevertVolume(r.Context(), req.target(), req.MountPath)
	if err != nil {
		writeVolumeError(w, r, err)
		return
	}
	slog.Info("operatorapi -> volume grow reverted", "target", req.serviceTarget,
		"mount", req.MountPath, "size_bytes", vol.DesiredSizeBytes)
	writeJSON(w, http.StatusOK, volumeRevertedJSON{OK: true, DesiredSizeBytes: vol.DesiredSizeBytes})
}

// --- Validation -------------------------------------------------------------

func (t serviceTarget) target() target.Target {
	return target.Target{Project: t.Project, Environment: t.Environment, Service: t.Service}
}

func (t serviceTarget) validate() error {
	if t.Project == "" || t.Environment == "" || t.Service == "" {
		return errors.New("project, environment and service are required")
	}
	return nil
}

// validate rejects what the schema's CHECK constraints would only catch as a
// 500. Sizing must be positive; the replica map must name at least one region.
func (r deployRequest) validate() error {
	if err := r.serviceTarget.validate(); err != nil {
		return err
	}
	if r.ImageRef == "" {
		return errors.New("image_ref is required")
	}
	return r.deploySpec.validate()
}

func (d deploySpec) validate() error {
	if d.CPUMillicores <= 0 || d.MemBytes <= 0 {
		return errors.New("cpu_millicores and mem_bytes must be positive")
	}
	if d.DrainSeconds < 0 || d.RestartMax < 0 || d.ProgressDeadline < 0 {
		return errors.New("drain_seconds, restart_max and progress_deadline must be >= 0")
	}
	return validateReplicas(d.Replicas)
}

func (d deploySpec) input(t target.Target, imageRef string) project.DeployInput {
	return project.DeployInput{
		Target:           t,
		ImageRef:         imageRef,
		CPUMillicores:    d.CPUMillicores,
		MemBytes:         d.MemBytes,
		DrainSeconds:     d.DrainSeconds,
		RestartMax:       d.RestartMax,
		ProgressDeadline: d.ProgressDeadline,
		Replicas:         d.Replicas,
		CommitMessage:    d.CommitMessage,
		CreatedBy:        d.CreatedBy,
	}
}

// validate orders the launch: a deploy needs a binding to land on, and the
// binding's image is what it deploys — the UI builds nothing from a repo.
func (r createServiceRequest) validate() error {
	if r.Project == "" || r.Name == "" {
		return errors.New("project and name are required")
	}
	if r.Deploy == nil {
		return nil
	}
	if r.Environment == "" {
		return errors.New("deploy needs an environment to bind the service into")
	}
	if r.Source.Image == "" {
		return errors.New("deploy needs source.image")
	}
	return r.Deploy.validate()
}

func (r scaleRequest) validate() error {
	if err := r.serviceTarget.validate(); err != nil {
		return err
	}
	if r.CPUMillicores < 0 || r.MemBytes < 0 {
		return errors.New("cpu_millicores and mem_bytes must be positive when given")
	}
	return validateReplicas(r.Replicas)
}

func (r volumeResizeRequest) validate() error {
	if err := r.serviceTarget.validate(); err != nil {
		return err
	}
	if r.MountPath == "" {
		return errors.New("mount_path is required")
	}
	if r.SizeBytes <= 0 {
		return errors.New("size_bytes must be positive")
	}
	return nil
}

func (r volumeRevertRequest) validate() error {
	if err := r.serviceTarget.validate(); err != nil {
		return err
	}
	if r.MountPath == "" {
		return errors.New("mount_path is required")
	}
	return nil
}

func validateReplicas(replicas map[string]int32) error {
	if len(replicas) == 0 {
		return errors.New("replicas must name at least one region")
	}
	for region, count := range replicas {
		if region == "" {
			return errors.New("replicas contains an empty region name")
		}
		if count < 0 || count > deployspec.MaxReplicas {
			return fmt.Errorf("replicas for %q must be between 0 and %d", region, deployspec.MaxReplicas)
		}
	}
	return nil
}

// writeDomainError maps the project layer's sentinels onto status codes; only a
// genuinely unexpected failure reaches the client as a 500.
func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, storage.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, project.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeInternalError(w, r, err)
	}
}

// writeVolumeError differs from writeDomainError in one mapping: every
// project.ErrInvalid a volume guard raises is a state conflict (a grow already
// in flight, nothing to revert, shrink of a converged disk), not a malformed
// request, so it is a 409 like restartReplica's — and the message goes through
// verbatim, since it names the state and the way out ("revert first").
func writeVolumeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, project.ErrInvalid):
		writeError(w, http.StatusConflict, err)
	default:
		writeInternalError(w, r, err)
	}
}

func orDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
