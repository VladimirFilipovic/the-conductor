package api

import (
	"database/sql"
	"net/http"
	"testing"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

func TestTopologyBuildsTree(t *testing.T) {
	envID, esID := pinnedID(1), pinnedID(2)
	depID, hostID := pinnedID(3), pinnedID(4)

	store := &fakeOperatorStore{
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
		volumes: []db.TopologyVolumesRow{{
			ID: pinnedID(8), MountPath: "/data", Region: "us-east-1",
			HostID: uuid.NullUUID{UUID: hostID, Valid: true}, Hostname: nullString("h1"),
			Status: "resize_pending", DesiredSizeBytes: 100 << 30,
			ObservedSizeBytes:        sql.NullInt64{Int64: 4 << 30, Valid: true},
			PreviousDesiredSizeBytes: sql.NullInt64{Int64: 4 << 30, Valid: true},
			EsID:                     esID,
		}},
		hosts: []db.Host{{ID: hostID, Hostname: "h1", Region: "us-east-1", HostHealthy: true, Status: "open"}},
	}
	api := NewOperatorAPI(store, &fakeDesired{})

	rec := do(t, api, http.MethodGet, "/v1/topology?project=acme&environment=all", "")
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
	if svc.EnvironmentServiceID != esID {
		t.Errorf("environment_service_id = %s, want %s", svc.EnvironmentServiceID, esID)
	}
	if svc.Deployment == nil || svc.Deployment.Version != 3 {
		t.Fatalf("deployment = %+v", svc.Deployment)
	}
	if len(svc.Replicas) != 2 {
		t.Errorf("replicas = %d, want 2 (both versions listed)", len(svc.Replicas))
	}
	if svc.Replicas[0].DeploymentVersion != 3 {
		t.Errorf("deployment_version = %d, want 3", svc.Replicas[0].DeploymentVersion)
	}
	if len(svc.Volumes) != 1 || svc.Volumes[0].MountPath != "/data" || svc.Volumes[0].Status != "resize_pending" {
		t.Fatalf("volumes = %+v, want the /data volume under its environment service", svc.Volumes)
	}
	if vol := svc.Volumes[0]; vol.Hostname == nil || *vol.Hostname != "h1" || vol.ObservedSizeBytes == nil || *vol.ObservedSizeBytes != 4<<30 {
		t.Errorf("volume = %+v, want host joined and observed size relayed", vol)
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
	store := &fakeOperatorStore{
		projects:     []string{"acme"},
		environments: []db.ListEnvironmentRowsRow{{ID: envID, ProjectName: "acme", Name: "production"}},
		services:     []db.TopologyServicesRow{{EsID: pinnedID(2), EnvironmentID: envID, Service: "web"}},
	}
	rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodGet, "/v1/topology", "")

	topo := decodeBody[topologyJSON](t, rec)
	svc := topo.Tree[0].Environments[0].Services[0]
	if svc.Deployment != nil {
		t.Errorf("deployment = %+v, want nil", svc.Deployment)
	}
	if svc.Volumes == nil {
		t.Error("volumes = null, want [] (stateless services carry an empty array)")
	}
	if svc.Replicas == nil || svc.Regions == nil {
		t.Error("replicas/regions = null, want []")
	}
}

// A current replica left in a region the deployment no longer targets still
// gets a summary row (desired 0), sorted with the rest.
func TestRegionSummariesKeepOrphanRegion(t *testing.T) {
	desired := map[string]int32{"us-east-1": 2}
	reps := []topologyReplicaJSON{
		{Region: "eu-west-1", IsCurrent: true, DesiredStatus: "running", Healthy: true},
		{Region: "us-east-1", IsCurrent: true, DesiredStatus: "running"},
		{Region: "ap-south-1", IsCurrent: false, DesiredStatus: "running", Healthy: true}, // old version: ignored
	}
	got := regionSummaries(desired, reps)
	want := []regionSummaryJSON{
		{Region: "eu-west-1", Desired: 0, Observed: 1, Healthy: 1},
		{Region: "us-east-1", Desired: 2, Observed: 1, Healthy: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("summaries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("summary[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMeta(t *testing.T) {
	store := &fakeOperatorStore{
		projects:     []string{"acme"},
		regions:      []string{"eu-west-1", "us-east-1"},
		environments: []db.ListEnvironmentRowsRow{{ID: pinnedID(1), ProjectName: "acme", Name: "production"}},
	}
	rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodGet, "/v1/meta", "")
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
