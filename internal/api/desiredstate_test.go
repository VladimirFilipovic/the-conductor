package api

import (
	"net/http"
	"testing"

	"conductor/internal/project"
	"conductor/internal/storage"

	"github.com/google/uuid"
)

func TestListServicesRequiresProject(t *testing.T) {
	rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, &fakeDesired{}), http.MethodGet, "/v1/services", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeployCommitsThroughProjectLayer(t *testing.T) {
	desired := &fakeDesired{}
	api := NewOperatorAPI(&fakeOperatorStore{}, desired)

	body := `{"project":"acme","environment":"production","service":"web",
	          "image_ref":"nginx:1.27","cpu_millicores":500,"mem_bytes":536870912,
	          "drain_seconds":30,"restart_max":5,"progress_deadline":600,
	          "commit_message":"via ui","created_by":"chaos-ui",
	          "replicas":{"us-east-1":2,"eu-west-1":1}}`
	rec := do(t, api, http.MethodPost, "/v1/deployments", body)
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

	res := decodeBody[deployedJSON](t, rec)
	if res.Version != 7 || res.Replicas["us-east-1"] != 2 {
		t.Errorf("response = %+v, want version 7 and the replica map echoed", res)
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
	api := NewOperatorAPI(&fakeOperatorStore{}, desired)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, api, http.MethodPost, "/v1/deployments", tc.body)
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
			rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, desired), http.MethodPost, "/v1/projects", tc.body)
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
	rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, desired), http.MethodPost, "/v1/projects", `{"name":"acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if desired.createdEnv != "production" {
		t.Errorf("environment = %q, want production", desired.createdEnv)
	}
	res := decodeBody[projectCreatedJSON](t, rec)
	if res.Environment != "production" {
		t.Errorf("response environment = %q, want the applied default", res.Environment)
	}
}

// The stateful single-instance rule lives in the project layer; the handler
// must relay it as a 400, not swallow it as a 500.
func TestScaleRelaysDomainRejection(t *testing.T) {
	desired := &fakeDesired{err: project.ErrInvalid}
	body := `{"project":"acme","environment":"production","service":"pg","replicas":{"us-east-1":3}}`
	rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, desired), http.MethodPost, "/v1/deployments/scale", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if desired.scale.Service != "pg" {
		t.Errorf("scale input = %+v", desired.scale)
	}
}

func TestScaleRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"partial target", `{"project":"acme","replicas":{"us-east-1":1}}`},
		{"no regions", `{"project":"acme","environment":"production","service":"web","replicas":{}}`},
		{"empty region name", `{"project":"acme","environment":"production","service":"web","replicas":{"":1}}`},
	}
	desired := &fakeDesired{}
	api := NewOperatorAPI(&fakeOperatorStore{}, desired)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, api, http.MethodPost, "/v1/deployments/scale", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
		})
	}
	if desired.scale.Service != "" {
		t.Error("a rejected scale still reached the project layer")
	}
}

func TestBindServiceRejectsBadIDs(t *testing.T) {
	rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, &fakeDesired{}), http.MethodPost, "/v1/environment-services",
		`{"environment_id":"nope","service_id":"`+uuid.Nil.String()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
