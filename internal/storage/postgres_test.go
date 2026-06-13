package storage

import (
	"context"
	"errors"
	"os"
	"testing"
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
// environment_services tree the domain creates, asserting each row links to its
// parent.
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
