package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
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
	// volumes reference environment_services ON DELETE RESTRICT, so they block
	// the project delete; clear them first. t.Cleanup is LIFO, so this
	// (registered last) runs before the project cleanup below.
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })
	prod, staging := bindInTwoEnvironments(t, c, proj.Name, "pg")

	v, err := c.CreateVolume(ctx, prod.ID, "data", "us-east-1", "/data", 1<<30)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v.MountPath != "/data" || v.DesiredSizeBytes != 1<<30 || v.EnvironmentServiceID != prod.ID {
		t.Fatalf("CreateVolume = %+v, want mount /data size 1GiB owned by %s", v, prod.ID)
	}

	if _, err := c.CreateVolume(ctx, prod.ID, "data2", "us-east-1", "/data", 1<<30); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate mount error = %v, want ErrExists", err)
	}
	// The same mount on the same service in another environment is a different
	// disk: the uniqueness is per binding, not per service.
	if _, err := c.CreateVolume(ctx, staging.ID, "data", "us-east-1", "/data", 1<<30); err != nil {
		t.Fatalf("CreateVolume in staging: %v", err)
	}

	for _, env := range []string{"production", "staging"} {
		vols, err := c.ListVolumesByEnvironmentService(ctx, proj.Name, env, "pg")
		if err != nil {
			t.Fatalf("ListVolumesByEnvironmentService(%s): %v", env, err)
		}
		if len(vols) != 1 {
			t.Fatalf("ListVolumesByEnvironmentService(%s) len = %d, want 1", env, len(vols))
		}
	}

	resized, err := c.UpdateVolumeSize(ctx, prod.ID, "/data", 5<<30)
	if err != nil {
		t.Fatalf("UpdateVolumeSize: %v", err)
	}
	if resized.DesiredSizeBytes != 5<<30 {
		t.Fatalf("UpdateVolumeSize = %d, want 5GiB", resized.DesiredSizeBytes)
	}
	if _, err := c.UpdateVolumeSize(ctx, prod.ID, "/missing", 5<<30); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resize missing volume error = %v, want ErrNotFound", err)
	}
	// Keyed by the binding: staging's copy is untouched by production's grow.
	if sv, err := c.GetVolume(ctx, staging.ID, "/data"); err != nil || sv.DesiredSizeBytes != 1<<30 {
		t.Fatalf("staging volume after production resize = %+v, %v; want 1GiB", sv, err)
	}

	if _, err := c.DeleteVolume(ctx, prod.ID, "/data"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := c.DeleteVolume(ctx, prod.ID, "/data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing volume error = %v, want ErrNotFound", err)
	}
	if _, err := c.DeleteVolume(ctx, staging.ID, "/data"); err != nil {
		t.Fatalf("DeleteVolume in staging: %v", err)
	}
}

// bindInTwoEnvironments creates one stateful service and binds it into a
// production and a staging environment — the shape the volume ownership
// change is about. Volumes hang off the returned bindings and are removed
// before the project cleanup (LIFO).
func bindInTwoEnvironments(t *testing.T, c *PostgresClient, project, service string) (prod, staging db.EnvironmentService) {
	t.Helper()
	ctx := context.Background()
	svc, err := c.CreateService(ctx, project, service, true)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	bind := func(name string) db.EnvironmentService {
		env, err := c.CreateEnvironment(ctx, project, name)
		if err != nil {
			t.Fatalf("CreateEnvironment(%s): %v", name, err)
		}
		es, err := c.AddServiceToEnvironment(ctx, env.ID, svc.ID, nil)
		if err != nil {
			t.Fatalf("AddServiceToEnvironment(%s): %v", name, err)
		}
		t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM volumes WHERE environment_service_id = $1", es.ID) })
		return es
	}
	return bind("production"), bind("staging")
}

// TestVolumeResizeGuards covers the resize state machine's SQL side: the two
// status flips only land while their predicate holds, the observed size is
// recorded verbatim, and the host commitment sums desired sizes.
func TestVolumeResizeGuards(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })
	es, _ := bindInTwoEnvironments(t, c, proj.Name, "pg")

	hosts, err := c.ListAgentHosts(ctx)
	if err != nil || len(hosts) == 0 {
		t.Fatalf("ListAgentHosts: %v (need the seeded fleet: make seed)", err)
	}
	host := hosts[0]

	v, err := c.CreateVolume(ctx, es.ID, "data", host.Region, "/data", 2<<30)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := c.AssignVolumeHost(ctx, v.ID, host.ID); err != nil {
		t.Fatalf("AssignVolumeHost: %v", err)
	}

	// Never observed: no drift, neither the park nor the approve predicate
	// fires; update still remembers the size it replaced.
	upd, err := c.UpdateVolumeSize(ctx, es.ID, "/data", 4<<30)
	if err != nil {
		t.Fatalf("UpdateVolumeSize: %v", err)
	}
	if !upd.PreviousDesiredSizeBytes.Valid || upd.PreviousDesiredSizeBytes.Int64 != 2<<30 || upd.Status != "attached" {
		t.Fatalf("after update = previous %v status %s, want previous 2GiB, status untouched", upd.PreviousDesiredSizeBytes, upd.Status)
	}
	if err := c.MarkVolumeResizePending(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeResizePending on never-observed = %v, want ErrConflict", err)
	}
	if err := c.MarkVolumeResizing(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeResizing on never-observed = %v, want ErrConflict", err)
	}
	// Not resize_pending: nothing to revert yet.
	if _, err := c.RevertVolumeSize(ctx, es.ID, "/data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevertVolumeSize while attached = %v, want ErrNotFound", err)
	}

	// Drifting without room: park, then revert, then settle.
	if err := c.RecordVolumeObservedSize(ctx, v.ID, 2<<30); err != nil {
		t.Fatalf("RecordVolumeObservedSize: %v", err)
	}
	if err := c.MarkVolumeResizePending(ctx, v.ID); err != nil {
		t.Fatalf("MarkVolumeResizePending with drift: %v", err)
	}
	if err := c.MarkVolumeResizePending(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeResizePending twice = %v, want ErrConflict (no longer attached)", err)
	}
	if err := c.MarkVolumeAttached(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeAttached on drifting resize_pending = %v, want ErrConflict", err)
	}
	rev, err := c.RevertVolumeSize(ctx, es.ID, "/data")
	if err != nil {
		t.Fatalf("RevertVolumeSize in resize_pending: %v", err)
	}
	if rev.DesiredSizeBytes != 2<<30 || rev.PreviousDesiredSizeBytes.Valid || rev.Status != "resize_pending" {
		t.Fatalf("after revert = desired %d previous %v status %s, want 2GiB, NULL, status untouched",
			rev.DesiredSizeBytes, rev.PreviousDesiredSizeBytes, rev.Status)
	}
	if _, err := c.RevertVolumeSize(ctx, es.ID, "/data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second RevertVolumeSize = %v, want ErrNotFound (one-shot)", err)
	}
	if err := c.MarkVolumeAttached(ctx, v.ID); err != nil {
		t.Fatalf("MarkVolumeAttached settling a reverted resize_pending: %v", err)
	}

	// Drifting with room: approve straight from attached, resizing is
	// committed (no revert), settle once the agent catches up.
	if _, err := c.UpdateVolumeSize(ctx, es.ID, "/data", 4<<30); err != nil {
		t.Fatalf("UpdateVolumeSize: %v", err)
	}
	if err := c.MarkVolumeResizing(ctx, v.ID); err != nil {
		t.Fatalf("MarkVolumeResizing with drift: %v", err)
	}
	if err := c.MarkVolumeResizing(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeResizing twice = %v, want ErrConflict (already resizing)", err)
	}
	if _, err := c.RevertVolumeSize(ctx, es.ID, "/data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevertVolumeSize while resizing = %v, want ErrNotFound", err)
	}
	if err := c.MarkVolumeAttached(ctx, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkVolumeAttached before catch-up = %v, want ErrConflict", err)
	}
	if err := c.RecordVolumeObservedSize(ctx, v.ID, 4<<30); err != nil {
		t.Fatalf("RecordVolumeObservedSize: %v", err)
	}
	if err := c.MarkVolumeAttached(ctx, v.ID); err != nil {
		t.Fatalf("MarkVolumeAttached after catch-up: %v", err)
	}
	got, err := c.GetVolume(ctx, es.ID, "/data")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Status != "attached" || !got.ObservedSizeBytes.Valid || got.ObservedSizeBytes.Int64 != 4<<30 {
		t.Fatalf("settled volume = status %s observed %v, want attached at 4GiB", got.Status, got.ObservedSizeBytes)
	}
	if sz := SizingOf(got); !sz.CaughtUp() || sz.Committed() != 4<<30 {
		t.Fatalf("SizingOf(settled) = %+v, want caught up and committing 4GiB", sz)
	}

	byHost, err := c.ListVolumesByHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("ListVolumesByHost: %v", err)
	}
	found := false
	for _, hv := range byHost {
		found = found || hv.ID == v.ID
	}
	if !found {
		t.Fatalf("ListVolumesByHost(%s) missing %s", host.ID, v.ID)
	}

	h, err := c.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if h.DiskBytes != host.DiskBytes {
		t.Fatalf("GetHost disk = %d, want %d", h.DiskBytes, host.DiskBytes)
	}
	if _, err := c.GetHost(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetHost unknown = %v, want ErrNotFound", err)
	}
	if _, err := c.GetVolume(ctx, es.ID, "/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVolume missing = %v, want ErrNotFound", err)
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
