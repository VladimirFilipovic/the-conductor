package api

import (
	"context"
	"encoding/json"
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

// DesiredState is the desired-state half of the control plane: exactly the
// project-domain operations the operator UI drives, and nothing else.
// *project.Service satisfies it. Every rule these writes must obey (one-current
// deployment, the single-instance cap on stateful services, the transaction
// boundaries) lives behind this interface — the handlers only translate JSON.
type DesiredState interface {
	Create(ctx context.Context, name, env string) (db.Project, error)
	CreateEnvironment(ctx context.Context, projectName, sourceEnv, name string) (project.CreateEnvironmentResult, error)
	CreateService(ctx context.Context, projectName, name string, stateful bool) (db.Service, error)
	BindService(ctx context.Context, in project.BindServiceInput) (db.EnvironmentService, error)
	Deploy(ctx context.Context, in project.DeployInput) (project.DeployResult, error)
	Scale(ctx context.Context, in project.ScaleInput) error
}

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
	ImageRef         string           `json:"image_ref"`
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
}

func (c *ControlPlane) createProject(w http.ResponseWriter, r *http.Request) {
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
	if _, err := c.desired.Create(r.Context(), req.Name, env); err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> project created", "project", req.Name, "environment", env)
	writeJSON(w, http.StatusCreated, createProjectRequest{Name: req.Name, Environment: env})
}

func (c *ControlPlane) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req createEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Project == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("project and name are required"))
		return
	}
	res, err := c.desired.CreateEnvironment(r.Context(), req.Project, req.SourceEnvironment, req.Name)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> environment created", "project", req.Project, "environment", req.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":                 res.Environment.ID,
		"project":            req.Project,
		"name":               res.Environment.Name,
		"source_environment": res.SourceEnv,
		"services_cloned":    res.ServicesCloned,
	})
}

func (c *ControlPlane) createService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Project == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("project and name are required"))
		return
	}
	svc, err := c.desired.CreateService(r.Context(), req.Project, req.Name, req.Stateful)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> service created", "project", req.Project, "service", req.Name)
	writeJSON(w, http.StatusCreated, serviceJSON{ID: svc.ID, Name: svc.Name, Stateful: svc.Stateful})
}

func (c *ControlPlane) bindService(w http.ResponseWriter, r *http.Request) {
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
	es, err := c.desired.BindService(r.Context(), project.BindServiceInput{
		EnvironmentID: environmentID, ServiceID: serviceID, Source: req.Source,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> service bound", "environment", environmentID, "service", serviceID)
	writeJSON(w, http.StatusCreated, map[string]uuid.UUID{"id": es.ID})
}

func (c *ControlPlane) deploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := c.desired.Deploy(r.Context(), project.DeployInput{
		Target:           req.target(),
		ImageRef:         req.ImageRef,
		CPUMillicores:    req.CPUMillicores,
		MemBytes:         req.MemBytes,
		DrainSeconds:     req.DrainSeconds,
		RestartMax:       req.RestartMax,
		ProgressDeadline: req.ProgressDeadline,
		Replicas:         req.Replicas,
		CommitMessage:    req.CommitMessage,
		CreatedBy:        req.CreatedBy,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> deployed", "target", req.serviceTarget, "version", res.Version)
	writeJSON(w, http.StatusCreated, map[string]any{"version": res.Version, "replicas": res.Replicas})
}

func (c *ControlPlane) scale(w http.ResponseWriter, r *http.Request) {
	var req scaleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateReplicas(req.Replicas); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := c.desired.Scale(r.Context(), project.ScaleInput{Target: req.target(), Replicas: req.Replicas}); err != nil {
		writeDomainError(w, err)
		return
	}
	slog.Info("controlplane -> scaled", "target", req.serviceTarget, "replicas", req.Replicas)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

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
	if r.CPUMillicores <= 0 || r.MemBytes <= 0 {
		return errors.New("cpu_millicores and mem_bytes must be positive")
	}
	if r.DrainSeconds < 0 || r.RestartMax < 0 || r.ProgressDeadline < 0 {
		return errors.New("drain_seconds, restart_max and progress_deadline must be >= 0")
	}
	if len(r.Replicas) == 0 {
		return errors.New("replicas must name at least one region")
	}
	return validateReplicas(r.Replicas)
}

func validateReplicas(replicas map[string]int32) error {
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

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return false
	}
	return true
}

// writeDomainError maps the project layer's sentinels onto status codes; only a
// genuinely unexpected failure reaches the client as a 500.
func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, storage.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, project.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func orDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
