package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"conductor/internal/storage"

	"github.com/google/uuid"
)

func TestDeploymentReplicasBadID(t *testing.T) {
	rec := do(t, NewOperatorAPI(&fakeOperatorStore{}, &fakeDesired{}), http.MethodGet, "/v1/deployments/not-a-uuid/replicas", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeploymentReplicasEmpty(t *testing.T) {
	api := NewOperatorAPI(&fakeOperatorStore{}, &fakeDesired{})
	rec := do(t, api, http.MethodGet, "/v1/deployments/"+uuid.Nil.String()+"/replicas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"replica_ids":[]}` {
		t.Errorf("body = %s, want an empty array", got)
	}
}

func TestDeleteReplica(t *testing.T) {
	store := &fakeOperatorStore{}
	api := NewOperatorAPI(store, &fakeDesired{})

	rec := do(t, api, http.MethodDelete, "/v1/replicas/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status = %d, want 400", rec.Code)
	}

	rec = do(t, api, http.MethodDelete, "/v1/replicas/"+pinnedID(9).String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(store.deleted) != 1 || store.deleted[0] != pinnedID(9) {
		t.Errorf("deleted = %v, want [%s]", store.deleted, pinnedID(9))
	}
}

// Restart maps the storage verdicts one-to-one: thawed → 200, unknown row →
// 404, exists but not failed → 409, anything else → 500 with the cause kept
// server-side.
func TestRestartReplica(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		storeErr error
		want     int
	}{
		{name: "bad id", path: "/v1/replicas/not-a-uuid/restart", want: http.StatusBadRequest},
		{name: "thawed", path: "/v1/replicas/" + pinnedID(9).String() + "/restart", want: http.StatusOK},
		{name: "unknown replica", path: "/v1/replicas/" + pinnedID(9).String() + "/restart", storeErr: storage.ErrNotFound, want: http.StatusNotFound},
		{name: "not failed", path: "/v1/replicas/" + pinnedID(9).String() + "/restart", storeErr: storage.ErrConflict, want: http.StatusConflict},
		{name: "storage failure", path: "/v1/replicas/" + pinnedID(9).String() + "/restart", storeErr: errors.New("boom"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeOperatorStore{restartErr: tt.storeErr}
			rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodPost, tt.path, "")
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusBadRequest {
				if len(store.restarted) != 0 {
					t.Errorf("bad id reached the store: %v", store.restarted)
				}
				return
			}
			if len(store.restarted) != 1 || store.restarted[0] != pinnedID(9) {
				t.Errorf("restarted = %v, want [%s]", store.restarted, pinnedID(9))
			}
			if tt.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "boom") {
				t.Errorf("driver text leaked to the client: %s", rec.Body)
			}
		})
	}
}
