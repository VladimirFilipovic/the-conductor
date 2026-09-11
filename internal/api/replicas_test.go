package api

import (
	"net/http"
	"strings"
	"testing"

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
