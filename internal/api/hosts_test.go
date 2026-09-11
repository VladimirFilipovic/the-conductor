package api

import (
	"net/http"
	"strings"
	"testing"
)

// One adapter serves cordon/uncordon/drain; the mapping is tested once through
// cordon and the other two routes are checked to reach the store at all.
func TestHostTransitionStatusMapping(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		applied bool
		err     error
		want    int
	}{
		{"bad id", "not-a-uuid", true, nil, http.StatusBadRequest},
		{"not applicable", pinnedID(1).String(), false, nil, http.StatusConflict},
		{"applied", pinnedID(1).String(), true, nil, http.StatusOK},
		{"store error", pinnedID(1).String(), false, errBoom, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeOperatorStore{transitionApplied: tc.applied, transitionErr: tc.err}
			rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodPost, "/v1/hosts/"+tc.id+"/cordon", "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// A 500 must not echo the driver's error to the client: the cause is for the
// server log, the client gets a stable generic message.
func TestInternalErrorHidesCause(t *testing.T) {
	store := &fakeOperatorStore{transitionErr: errBoom}
	rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodPost, "/v1/hosts/"+pinnedID(1).String()+"/drain", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "relation") {
		t.Errorf("body leaks the storage error: %s", rec.Body)
	}
	if got := decodeBody[errorJSON](t, rec); got.Error != "internal error" {
		t.Errorf("error = %q, want the generic message", got.Error)
	}
}

func TestHostTransitionRoutes(t *testing.T) {
	for _, route := range []string{"cordon", "uncordon", "drain"} {
		store := &fakeOperatorStore{transitionApplied: true}
		rec := do(t, NewOperatorAPI(store, &fakeDesired{}), http.MethodPost, "/v1/hosts/"+pinnedID(2).String()+"/"+route, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, body %s", route, rec.Code, rec.Body)
		}
		if len(store.transitioned) != 1 || store.transitioned[0] != pinnedID(2) {
			t.Errorf("%s: store saw %v, want [%s]", route, store.transitioned, pinnedID(2))
		}
	}
}
