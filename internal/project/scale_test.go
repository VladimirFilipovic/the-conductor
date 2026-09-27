package project_test

import (
	"context"
	"encoding/json"
	"testing"

	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
)

// scaleStore is an in-memory current deployment (v4: 500m / 512 MiB, two
// regions) plus a record of what Scale wrote back. Like fakeStore, anything
// outside the scale/resize path panics through the nil embedded Querier.
type scaleStore struct {
	storage.Querier

	current db.Deployment
	regions []db.ListDeploymentRegionsRow

	created    *db.CreateDeploymentParams
	superseded bool
	setRegions map[uuid.UUID]map[string]int32
}

func newScaleStore() *scaleStore {
	return &scaleStore{
		current: db.Deployment{
			ID: uuid.New(), Version: 4, ImageRef: "nginx:1.27",
			CpuMillicores: 500, MemBytes: 512 << 20,
			Env:          json.RawMessage(`{"FOO":"bar"}`),
			Healthcheck:  json.RawMessage(`{"path":"/healthz","timeout_s":5}`),
			DrainSeconds: 45, RestartMax: 3, ProgressDeadline: 900,
		},
		regions: []db.ListDeploymentRegionsRow{
			{Region: "eu-west-1", Replicas: 1},
			{Region: "us-east-1", Replicas: 2},
		},
		setRegions: map[uuid.UUID]map[string]int32{},
	}
}

func (f *scaleStore) WithTx(_ context.Context, fn func(storage.Querier) error) error { return fn(f) }

func (f *scaleStore) GetService(_ context.Context, _, name string) (db.Service, error) {
	return db.Service{Name: name}, nil
}

func (f *scaleStore) GetEnvironmentService(_ context.Context, _, _, _ string) (db.GetEnvironmentServiceRow, error) {
	return db.GetEnvironmentServiceRow{ID: uuid.New()}, nil
}

func (f *scaleStore) CurrentDeploymentID(_ context.Context, _, _, _ string) (uuid.UUID, error) {
	return f.current.ID, nil
}

func (f *scaleStore) GetCurrentDeploymentSpec(_ context.Context, _ uuid.UUID) (db.Deployment, error) {
	return f.current, nil
}

func (f *scaleStore) ListDeploymentRegions(_ context.Context, _ uuid.UUID) ([]db.ListDeploymentRegionsRow, error) {
	return f.regions, nil
}

func (f *scaleStore) NextDeploymentVersion(_ context.Context, _ uuid.UUID) (int32, error) {
	return f.current.Version + 1, nil
}

func (f *scaleStore) SupersedeCurrentDeployments(_ context.Context, _ uuid.UUID) error {
	f.superseded = true
	return nil
}

func (f *scaleStore) CreateDeployment(_ context.Context, arg db.CreateDeploymentParams) (db.Deployment, error) {
	f.created = &arg
	return db.Deployment{ID: uuid.New(), Version: arg.Version}, nil
}

func (f *scaleStore) SetDeploymentRegion(_ context.Context, id uuid.UUID, region string, n int32) error {
	if f.setRegions[id] == nil {
		f.setRegions[id] = map[string]int32{}
	}
	f.setRegions[id][region] = n
	return nil
}

func scaleTarget() target.Target {
	return target.Target{Project: "acme", Environment: "production", Service: "api"}
}

func TestScaleWithoutLimitsPatchesInPlace(t *testing.T) {
	f := newScaleStore()
	res, err := project.New(f).Scale(context.Background(), project.ScaleInput{
		Target: scaleTarget(), Replicas: map[string]int32{"us-east-1": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 0 || f.created != nil {
		t.Fatalf("version = %d, created = %v; a count-only scale must not commit", res.Version, f.created)
	}
	if got := f.setRegions[f.current.ID]["us-east-1"]; got != 3 {
		t.Errorf("us-east-1 = %d on the current deployment, want 3", got)
	}
}

// Unchanged limits are the UI's common case — the form always sends the
// prefilled values — and must stay an in-place scale, not a redeploy.
func TestScaleWithSameLimitsPatchesInPlace(t *testing.T) {
	f := newScaleStore()
	res, err := project.New(f).Scale(context.Background(), project.ScaleInput{
		Target: scaleTarget(), Replicas: map[string]int32{"us-east-1": 3},
		CPUMillicores: 500, MemBytes: 512 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 0 || f.created != nil {
		t.Fatalf("version = %d; same limits must not commit a new version", res.Version)
	}
}

func TestScaleWithNewLimitsRecommitsCurrentSpec(t *testing.T) {
	f := newScaleStore()
	res, err := project.New(f).Scale(context.Background(), project.ScaleInput{
		Target: scaleTarget(), Replicas: map[string]int32{"us-east-1": 3},
		CPUMillicores: 1000, CreatedBy: "chaos-ui",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 5 || !f.superseded || f.created == nil {
		t.Fatalf("version = %d, superseded = %v; want v5 replacing v4", res.Version, f.superseded)
	}

	c, cur := f.created, f.current
	if c.CpuMillicores != 1000 || c.MemBytes != cur.MemBytes {
		t.Errorf("limits = %dm / %d, want 1000m and the unchanged memory", c.CpuMillicores, c.MemBytes)
	}
	if c.ImageRef != cur.ImageRef || string(c.Env) != string(cur.Env) || string(c.Healthcheck) != string(cur.Healthcheck) {
		t.Errorf("spec drifted: image %q env %s healthcheck %s", c.ImageRef, c.Env, c.Healthcheck)
	}
	if c.DrainSeconds != 45 || c.RestartMax != 3 || c.ProgressDeadline != 900 {
		t.Errorf("rollout knobs = %d/%d/%d, want 45/3/900 carried over", c.DrainSeconds, c.RestartMax, c.ProgressDeadline)
	}

	var regions map[string]int32
	for id, r := range f.setRegions {
		if id != cur.ID {
			regions = r
		}
	}
	if regions["us-east-1"] != 3 || regions["eu-west-1"] != 1 {
		t.Errorf("new version's regions = %v, want us-east-1 patched to 3 and eu-west-1 kept at 1", regions)
	}
}
