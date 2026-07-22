package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"conductor/internal/storage/db"
)

// newTestClient connects to the DSN in CONDUCTOR_TEST_DSN, skipping the test
// when it is unset so the default `go test ./...` needs no database. The schema
// is owned by the goose migrations — run `make migrate` against the test DSN
// first.
func newTestClient(t *testing.T) *PostgresClient {
	t.Helper()
	dsn := os.Getenv("CONDUCTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("set CONDUCTOR_TEST_DSN to run Postgres storage tests")
	}
	ctx := context.Background()
	c, err := NewPostgresClient(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestProjectTreeRoundTrip walks the projects → environments → services →
// environment_services tree, asserting each row links to its parent.
func TestProjectTreeRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })

	env, err := c.CreateEnvironment(ctx, proj.Name, "production")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.ProjectName != proj.Name {
		t.Fatalf("environment.ProjectName = %s, want %s", env.ProjectName, proj.Name)
	}

	svc, err := c.CreateService(ctx, proj.Name, "web", false)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if svc.ProjectName != proj.Name {
		t.Fatalf("service.ProjectName = %s, want %s", svc.ProjectName, proj.Name)
	}

	es, err := c.AddServiceToEnvironment(ctx, env.ID, svc.ID, nil)
	if err != nil {
		t.Fatalf("AddServiceToEnvironment: %v", err)
	}
	if es.EnvironmentID != env.ID || es.ServiceID != svc.ID {
		t.Fatalf("environment_service = (%s,%s), want (%s,%s)", es.EnvironmentID, es.ServiceID, env.ID, svc.ID)
	}
}

// TestEnvironmentCloneAndList covers `environment create`/`list`: a fresh
// environment clones the source environment's service bindings, and the list is
// returned project-scoped and name-ordered.
func TestEnvironmentCloneAndList(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })

	prod, err := c.CreateEnvironment(ctx, proj.Name, "production")
	if err != nil {
		t.Fatalf("CreateEnvironment production: %v", err)
	}
	svc, err := c.CreateService(ctx, proj.Name, "web", false)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := c.AddServiceToEnvironment(ctx, prod.ID, svc.ID, nil); err != nil {
		t.Fatalf("AddServiceToEnvironment: %v", err)
	}

	staging, err := c.CreateEnvironment(ctx, proj.Name, "staging")
	if err != nil {
		t.Fatalf("CreateEnvironment staging: %v", err)
	}
	cloned, err := c.CloneEnvironmentServices(ctx, prod.ID, staging.ID)
	if err != nil {
		t.Fatalf("CloneEnvironmentServices: %v", err)
	}
	if cloned != 1 {
		t.Fatalf("cloned = %d, want 1", cloned)
	}

	envs, err := c.ListEnvironments(ctx, proj.Name)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 || envs[0].Name != "production" || envs[1].Name != "staging" {
		t.Fatalf("ListEnvironments = %v, want [production staging]", envs)
	}
}

// TestVolumeCRUD covers `volume add/list/update/rm`, including the duplicate and
// missing-volume error mappings.
func TestVolumeCRUD(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	svc, err := c.CreateService(ctx, proj.Name, "pg", true)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	// volumes reference services ON DELETE RESTRICT, so they block the project
	// delete; clear them first. t.Cleanup is LIFO, so this (registered last) runs
	// before the project cleanup below.
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM volumes WHERE service_id = $1", svc.ID) })

	v, err := c.CreateVolume(ctx, svc.ID, "data", "us-east-1", "/data", 1<<30)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v.MountPath != "/data" || v.DesiredSizeBytes != 1<<30 {
		t.Fatalf("CreateVolume = %+v, want mount /data size 1GiB", v)
	}

	if _, err := c.CreateVolume(ctx, svc.ID, "data2", "us-east-1", "/data", 1<<30); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate mount error = %v, want ErrExists", err)
	}

	vols, err := c.ListVolumesByService(ctx, proj.Name, "pg")
	if err != nil {
		t.Fatalf("ListVolumesByService: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("ListVolumesByService len = %d, want 1", len(vols))
	}

	resized, err := c.UpdateVolumeSize(ctx, svc.ID, "/data", 5<<30)
	if err != nil {
		t.Fatalf("UpdateVolumeSize: %v", err)
	}
	if resized.DesiredSizeBytes != 5<<30 {
		t.Fatalf("UpdateVolumeSize = %d, want 5GiB", resized.DesiredSizeBytes)
	}
	if _, err := c.UpdateVolumeSize(ctx, svc.ID, "/missing", 5<<30); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resize missing volume error = %v, want ErrNotFound", err)
	}

	if _, err := c.DeleteVolume(ctx, svc.ID, "/data"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := c.DeleteVolume(ctx, svc.ID, "/data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing volume error = %v, want ErrNotFound", err)
	}
}

// TestCurrentDeploymentScaleDown covers the reconciler's direct input: scale
// upserts a deployment_region for the current commit, down zeroes them, and an
// undeployed service resolves to ErrNotFound.
func TestCurrentDeploymentScaleDown(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })

	env, err := c.CreateEnvironment(ctx, proj.Name, "production")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	svc, err := c.CreateService(ctx, proj.Name, "web", false)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	if _, err := c.CurrentDeploymentID(ctx, proj.Name, "production", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CurrentDeploymentID (undeployed) = %v, want ErrNotFound", err)
	}

	es, err := c.AddServiceToEnvironment(ctx, env.ID, svc.ID, nil)
	if err != nil {
		t.Fatalf("AddServiceToEnvironment: %v", err)
	}
	dep, err := c.CreateDeployment(ctx, db.CreateDeploymentParams{
		EnvironmentServiceID: es.ID,
		Version:              1,
		ImageRef:             "nginx:latest",
		CpuMillicores:        500,
		MemBytes:             1 << 29,
		Env:                  json.RawMessage("{}"),
		Healthcheck:          json.RawMessage("{}"),
		DrainSeconds:         30,
		RestartMax:           5,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	got, err := c.CurrentDeploymentID(ctx, proj.Name, "production", "web")
	if err != nil {
		t.Fatalf("CurrentDeploymentID: %v", err)
	}
	if got != dep.ID {
		t.Fatalf("CurrentDeploymentID = %s, want %s", got, dep.ID)
	}

	if err := c.SetDeploymentRegion(ctx, dep.ID, "us-east-1", 3); err != nil {
		t.Fatalf("SetDeploymentRegion: %v", err)
	}
	assertDesiredReplicas(t, c, proj.Name, 3)

	if err := c.ZeroDeploymentRegions(ctx, dep.ID); err != nil {
		t.Fatalf("ZeroDeploymentRegions: %v", err)
	}
	assertDesiredReplicas(t, c, proj.Name, 0)
}

func assertDesiredReplicas(t *testing.T, c *PostgresClient, projectName string, want int32) {
	t.Helper()
	rows, err := c.ProjectStatus(context.Background(), projectName, "", "")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ProjectStatus rows = %d, want 1", len(rows))
	}
	if rows[0].DesiredReplicas != want {
		t.Fatalf("DesiredReplicas = %d, want %d", rows[0].DesiredReplicas, want)
	}
}

func TestCreateProjectRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })

	if _, err := c.CreateProject(ctx, t.Name()); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate CreateProject error = %v, want ErrExists", err)
	}
}
