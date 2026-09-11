package api

// Control-plane HTTP tests: the read assembly (flat rows in, tree out), one
// write path end to end, and the 4xx branches that keep a bad request from
// reaching the project layer at all.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// fakeCPStore serves canned rows. The embedded nil interface panics on any
// method a test does not stub, asserting each handler reaches for nothing more.
type fakeCPStore struct {
	ControlPlaneStore

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
}

func (f *fakeCPStore) ListProjectNames(_ context.Context, project string) ([]string, error) {
	f.gotFilter.Project = project
	return f.projects, nil
}

func (f *fakeCPStore) ListRegionNames(context.Context) ([]string, error) { return f.regions, nil }

func (f *fakeCPStore) ListEnvironmentRows(_ context.Context, _, _ string) ([]db.ListEnvironmentRowsRow, error) {
	return f.environments, nil
}

func (f *fakeCPStore) ListProjectServices(_ context.Context, _, _ string) ([]db.ListProjectServicesRow, error) {
	return f.projectSvcs, nil
}

func (f *fakeCPStore) TopologyServices(_ context.Context, filter storage.TopologyFilter) ([]db.TopologyServicesRow, error) {
	f.gotFilter = filter
	return f.services, nil
}

func (f *fakeCPStore) TopologyDesiredRegions(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyDesiredRegionsRow, error) {
	return f.desired, nil
}

func (f *fakeCPStore) TopologyReplicas(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyReplicasRow, error) {
	return f.replicas, nil
}

func (f *fakeCPStore) TopologyHosts(_ context.Context, _ string) ([]db.Host, error) {
	return f.hosts, nil
}

func (f *fakeCPStore) TopologyServed(_ context.Context, _ storage.TopologyFilter) ([]db.TopologyServedRow, error) {
	return f.served, nil
}

func (f *fakeCPStore) ListDeploymentReplicaIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return f.replicaIDs, nil
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

func (f *fakeDesired) Create(_ context.Context, name, env string) (db.Project, error) {
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

func do(t *testing.T, cp *ControlPlane, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	cp.Handler().ServeHTTP(rec, req)
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

func TestTopologyBuildsTree(t *testing.T) {
	envID, esID := pinnedID(1), pinnedID(2)
	depID, hostID := pinnedID(3), pinnedID(4)

	store := &fakeCPStore{
		projects:     []string{"acme"},
		environments: []db.ListEnvironmentRowsRow{{ID: envID, ProjectName: "acme", Name: "production"}},
		services: []db.TopologyServicesRow{{
			EsID: esID, EnvironmentID: envID, Service: "web", Stateful: false,
			DeploymentID: uuid.NullUUID{UUID: depID, Valid: true},
			Version:      sql.NullInt32{Int32: 3, Valid: true},
			Status:       nullString("active"), ImageRef: nullString("nginx:1.27"),
			CreatedAt: sql.NullTime{Time: fixedNow, Valid: true},
		}},
		desired: []db.TopologyDesiredRegionsRow{{EsID: esID, Region: "us-east-1", Desired: 2}},
		replicas: []db.TopologyReplicasRow{
			{
				ID: pinnedID(5), Region: "us-east-1", Hostname: nullString("h1"),
				HostID: uuid.NullUUID{UUID: hostID, Valid: true}, Phase: "active",
				Healthy: true, DesiredStatus: "running", UpdatedAt: fixedNow,
				DepVersion: 3, IsCurrent: true, DeploymentID: depID, EsID: esID,
			},
			// A surviving replica of the superseded version: it must show in the
			// replica list but never count toward the current version's health.
			{
				ID: pinnedID(6), Region: "us-east-1", Phase: "draining",
				DesiredStatus: "running", UpdatedAt: fixedNow,
				DepVersion: 2, IsCurrent: false, DeploymentID: pinnedID(7), EsID: esID,
			},
		},
		hosts: []db.Host{{ID: hostID, Hostname: "h1", Region: "us-east-1", Status: "ready"}},
	}
	cp := NewControlPlane(store, &fakeDesired{})

	rec := do(t, cp, http.MethodGet, "/v1/topology?project=acme&environment=all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	// "all" is the UI's word for "no filter" and must not reach the store.
	if store.gotFilter.Environment != "" {
		t.Errorf(`environment filter = %q, want "" for "all"`, store.gotFilter.Environment)
	}
	if store.gotFilter.Project != "acme" {
		t.Errorf("project filter = %q, want acme", store.gotFilter.Project)
	}

	topo := decodeBody[topologyJSON](t, rec)
	if len(topo.Tree) != 1 || len(topo.Tree[0].Environments) != 1 {
		t.Fatalf("tree = %+v", topo.Tree)
	}
	svcs := topo.Tree[0].Environments[0].Services
	if len(svcs) != 1 {
		t.Fatalf("services = %+v", svcs)
	}
	svc := svcs[0]
	if svc.Deployment == nil || svc.Deployment.Version != 3 {
		t.Fatalf("deployment = %+v", svc.Deployment)
	}
	if len(svc.Replicas) != 2 {
		t.Errorf("replicas = %d, want 2 (both versions listed)", len(svc.Replicas))
	}
	if len(svc.Regions) != 1 {
		t.Fatalf("regions = %+v", svc.Regions)
	}
	want := regionSummaryJSON{Region: "us-east-1", Desired: 2, Observed: 1, Healthy: 1}
	if svc.Regions[0] != want {
		t.Errorf("region summary = %+v, want %+v", svc.Regions[0], want)
	}
	if len(topo.Hosts) != 1 || topo.Hosts[0].Hostname != "h1" {
		t.Errorf("hosts = %+v", topo.Hosts)
	}
	if topo.Served == nil {
		t.Error("served = null, want [] (clients iterate it unguarded)")
	}
}

// A service bound but never deployed still has to appear, with a null deployment.
func TestTopologyUndeployedService(t *testing.T) {
	envID := pinnedID(1)
	store := &fakeCPStore{
		projects:     []string{"acme"},
		environments: []db.ListEnvironmentRowsRow{{ID: envID, ProjectName: "acme", Name: "production"}},
		services:     []db.TopologyServicesRow{{EsID: pinnedID(2), EnvironmentID: envID, Service: "web"}},
	}
	rec := do(t, NewControlPlane(store, &fakeDesired{}), http.MethodGet, "/v1/topology", "")

	topo := decodeBody[topologyJSON](t, rec)
	svc := topo.Tree[0].Environments[0].Services[0]
	if svc.Deployment != nil {
		t.Errorf("deployment = %+v, want nil", svc.Deployment)
	}
	if svc.Replicas == nil || svc.Regions == nil {
		t.Error("replicas/regions = null, want []")
	}
}

func TestMeta(t *testing.T) {
	store := &fakeCPStore{
		projects:     []string{"acme"},
		regions:      []string{"eu-west-1", "us-east-1"},
		environments: []db.ListEnvironmentRowsRow{{ID: pinnedID(1), ProjectName: "acme", Name: "production"}},
	}
	rec := do(t, NewControlPlane(store, &fakeDesired{}), http.MethodGet, "/v1/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	meta := decodeBody[metaJSON](t, rec)
	if len(meta.Projects) != 1 || meta.Projects[0].Name != "acme" {
		t.Errorf("projects = %+v", meta.Projects)
	}
	if len(meta.Regions) != 2 || len(meta.Environments) != 1 {
		t.Errorf("meta = %+v", meta)
	}
}

func TestListServicesRequiresProject(t *testing.T) {
	rec := do(t, NewControlPlane(&fakeCPStore{}, &fakeDesired{}), http.MethodGet, "/v1/services", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeploymentReplicasBadID(t *testing.T) {
	rec := do(t, NewControlPlane(&fakeCPStore{}, &fakeDesired{}), http.MethodGet, "/v1/deployments/not-a-uuid/replicas", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeploymentReplicasEmpty(t *testing.T) {
	cp := NewControlPlane(&fakeCPStore{}, &fakeDesired{})
	rec := do(t, cp, http.MethodGet, "/v1/deployments/"+uuid.Nil.String()+"/replicas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"replica_ids":[]}` {
		t.Errorf("body = %s, want an empty array", got)
	}
}

func TestDeployCommitsThroughProjectLayer(t *testing.T) {
	desired := &fakeDesired{}
	cp := NewControlPlane(&fakeCPStore{}, desired)

	body := `{"project":"acme","environment":"production","service":"web",
	          "image_ref":"nginx:1.27","cpu_millicores":500,"mem_bytes":536870912,
	          "drain_seconds":30,"restart_max":5,"progress_deadline":600,
	          "commit_message":"via ui","created_by":"chaos-ui",
	          "replicas":{"us-east-1":2,"eu-west-1":1}}`
	rec := do(t, cp, http.MethodPost, "/v1/deployments", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	got := desired.deploy
	if got.Project != "acme" || got.Environment != "production" || got.Service != "web" {
		t.Errorf("target = %+v", got.Target)
	}
	if got.ProgressDeadline != 600 || got.CPUMillicores != 500 || got.MemBytes != 536870912 {
		t.Errorf("spec = %+v", got)
	}
	if got.Replicas["us-east-1"] != 2 || got.Replicas["eu-west-1"] != 1 {
		t.Errorf("replicas = %+v", got.Replicas)
	}

	res := decodeBody[struct {
		Version int32 `json:"version"`
	}](t, rec)
	if res.Version != 7 {
		t.Errorf("version = %d, want 7", res.Version)
	}
}

func TestDeployRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"unknown field", `{"project":"a","environment":"e","service":"s","image_ref":"i","cpu_millicores":1,"mem_bytes":1,"replicas":{"r":1},"whoops":true}`},
		{"missing service", `{"project":"a","environment":"e","image_ref":"i","cpu_millicores":1,"mem_bytes":1,"replicas":{"r":1}}`},
		{"missing image", `{"project":"a","environment":"e","service":"s","cpu_millicores":1,"mem_bytes":1,"replicas":{"r":1}}`},
		{"zero sizing", `{"project":"a","environment":"e","service":"s","image_ref":"i","cpu_millicores":0,"mem_bytes":1,"replicas":{"r":1}}`},
		{"no regions", `{"project":"a","environment":"e","service":"s","image_ref":"i","cpu_millicores":1,"mem_bytes":1,"replicas":{}}`},
		{"absurd replica count", `{"project":"a","environment":"e","service":"s","image_ref":"i","cpu_millicores":1,"mem_bytes":1,"replicas":{"r":9999}}`},
	}
	desired := &fakeDesired{}
	cp := NewControlPlane(&fakeCPStore{}, desired)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, cp, http.MethodPost, "/v1/deployments", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
		})
	}
	if desired.deploy.ImageRef != "" {
		t.Error("a rejected request still reached the project layer")
	}
}

func TestCreateProjectStatusMapping(t *testing.T) {
	tests := []struct {
		name string
		body string
		err  error
		want int
	}{
		{"created", `{"name":"acme","environment":"staging"}`, nil, http.StatusCreated},
		{"missing name", `{"environment":"staging"}`, nil, http.StatusBadRequest},
		{"duplicate", `{"name":"acme"}`, storage.ErrExists, http.StatusConflict},
		{"unknown parent", `{"name":"acme"}`, storage.ErrNotFound, http.StatusNotFound},
		{"database down", `{"name":"acme"}`, errBoom, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			desired := &fakeDesired{err: tc.err}
			rec := do(t, NewControlPlane(&fakeCPStore{}, desired), http.MethodPost, "/v1/projects", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// A project is born with an environment; an omitted one falls back to the same
// default `conductor init` uses.
func TestCreateProjectDefaultEnvironment(t *testing.T) {
	desired := &fakeDesired{}
	rec := do(t, NewControlPlane(&fakeCPStore{}, desired), http.MethodPost, "/v1/projects", `{"name":"acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if desired.createdEnv != "production" {
		t.Errorf("environment = %q, want production", desired.createdEnv)
	}
}

// The stateful single-instance rule lives in the project layer; the handler
// must relay it as a 400, not swallow it as a 500.
func TestScaleRelaysDomainRejection(t *testing.T) {
	desired := &fakeDesired{err: project.ErrInvalid}
	body := `{"project":"acme","environment":"production","service":"pg","replicas":{"us-east-1":3}}`
	rec := do(t, NewControlPlane(&fakeCPStore{}, desired), http.MethodPost, "/v1/deployments/scale", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if desired.scale.Service != "pg" {
		t.Errorf("scale input = %+v", desired.scale)
	}
}

func TestScaleRequiresFullTarget(t *testing.T) {
	rec := do(t, NewControlPlane(&fakeCPStore{}, &fakeDesired{}), http.MethodPost, "/v1/deployments/scale",
		`{"project":"acme","replicas":{"us-east-1":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBindServiceRejectsBadIDs(t *testing.T) {
	rec := do(t, NewControlPlane(&fakeCPStore{}, &fakeDesired{}), http.MethodPost, "/v1/environment-services",
		`{"environment_id":"nope","service_id":"`+uuid.Nil.String()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// errBoom stands in for an unexpected storage failure — nothing the caller can
// fix, so it must surface as a 500 rather than any of the mapped 4xx codes.
var errBoom = errors.New("boom")
