package api

// Shared fixtures for the OperatorAPI HTTP tests. The per-resource tests
// (hosts, replicas, topology, desiredstate) live next to the handlers they
// cover.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// fakeOperatorStore serves canned rows. The embedded nil interface panics on
// any method a test does not stub, asserting each handler reaches for nothing
// more.
type fakeOperatorStore struct {
	OperatorAPIStore

	projects     []string
	regions      []string
	environments []db.ListEnvironmentRowsRow
	services     []db.TopologyServicesRow
	desired      []db.TopologyDesiredRegionsRow
	replicas     []db.TopologyReplicasRow
	hosts        []db.Host
	served       []db.TopologyServedRow
	projectSvcs  []db.ListProjectServicesRow
	replicaIDs   []uuid.UUID

	// Filters the last topology read was issued with.
	gotFilter storage.TopologyFilter

	// Host transitions answer with transitionApplied/transitionErr and record
	// the host they were asked about; deletions record the replica.
	transitionApplied bool
	transitionErr     error
	transitioned      []uuid.UUID
	deleted           []uuid.UUID
}

func (f *fakeOperatorStore) ListProjectNames(_ context.Context, project string) ([]string, error) {
	f.gotFilter.Project = project
	return f.projects, nil
}

func (f *fakeOperatorStore) ListRegionNames(context.Context) ([]string, error) { return f.regions, nil }

func (f *fakeOperatorStore) ListEnvironmentRows(_ context.Context, _, _ string) ([]db.ListEnvironmentRowsRow, error) {
	return f.environments, nil
}

func (f *fakeOperatorStore) ListProjectServices(_ context.Context, _, _ string) ([]db.ListProjectServicesRow, error) {
	return f.projectSvcs, nil
}

func (f *fakeOperatorStore) TopologyServices(_ context.Context, filter storage.TopologyFilter) ([]db.TopologyServicesRow, error) {
	f.gotFilter = filter
	return f.services, nil
}

func (f *fakeOperatorStore) TopologyDesiredRegions(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyDesiredRegionsRow, error) {
	return f.desired, nil
}

func (f *fakeOperatorStore) TopologyReplicas(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyReplicasRow, error) {
	return f.replicas, nil
}

func (f *fakeOperatorStore) TopologyHosts(_ context.Context, _ string) ([]db.Host, error) {
	return f.hosts, nil
}

func (f *fakeOperatorStore) TopologyServed(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyServedRow, error) {
	return f.served, nil
}

func (f *fakeOperatorStore) ListDeploymentReplicaIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return f.replicaIDs, nil
}

func (f *fakeOperatorStore) transition(hostID uuid.UUID) (bool, error) {
	f.transitioned = append(f.transitioned, hostID)
	return f.transitionApplied, f.transitionErr
}

func (f *fakeOperatorStore) CordonHost(_ context.Context, hostID uuid.UUID) (bool, error) {
	return f.transition(hostID)
}

func (f *fakeOperatorStore) UncordonHost(_ context.Context, hostID uuid.UUID) (bool, error) {
	return f.transition(hostID)
}

func (f *fakeOperatorStore) DrainHost(_ context.Context, hostID uuid.UUID) (bool, error) {
	return f.transition(hostID)
}

func (f *fakeOperatorStore) DeleteReplica(_ context.Context, replicaID uuid.UUID) error {
	f.deleted = append(f.deleted, replicaID)
	return f.transitionErr
}

// fakeDesired records what the handlers hand the project layer, and can answer
// with a domain error so the status mapping is exercised without a database.
type fakeDesired struct {
	err error

	createdProject string
	createdEnv     string
	deploy         project.DeployInput
	scale          project.ScaleInput
}

func (f *fakeDesired) CreateProject(_ context.Context, name, env string) (db.Project, error) {
	f.createdProject, f.createdEnv = name, env
	return db.Project{Name: name}, f.err
}

func (f *fakeDesired) CreateEnvironment(_ context.Context, _, _, name string) (project.CreateEnvironmentResult, error) {
	return project.CreateEnvironmentResult{Environment: db.Environment{ID: uuid.New(), Name: name}}, f.err
}

func (f *fakeDesired) CreateService(_ context.Context, _, name string, stateful bool) (db.Service, error) {
	return db.Service{ID: uuid.New(), Name: name, Stateful: stateful}, f.err
}

func (f *fakeDesired) BindService(context.Context, project.BindServiceInput) (db.EnvironmentService, error) {
	return db.EnvironmentService{ID: uuid.New()}, f.err
}

func (f *fakeDesired) Deploy(_ context.Context, in project.DeployInput) (project.DeployResult, error) {
	f.deploy = in
	return project.DeployResult{Version: 7, Replicas: in.Replicas}, f.err
}

func (f *fakeDesired) Scale(_ context.Context, in project.ScaleInput) error {
	f.scale = in
	return f.err
}

func do(t *testing.T, api *OperatorAPI, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// errBoom stands in for an unexpected storage failure — nothing the caller can
// fix, so it must surface as a 500 rather than any of the mapped 4xx codes.
var errBoom = errors.New("pq: relation \"hosts\" does not exist")
