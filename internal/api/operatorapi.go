package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// maxRequestBytes caps an operator request body. Every write here is a
// handful of scalars, so anything larger is a mistake or an attack.
const maxRequestBytes = 1 << 20

// FleetReader is the read side of the OperatorAPI: the fleet, the naming tree,
// and the desired-vs-observed slices the topology view joins together.
type FleetReader interface {
	ListAgentHosts(ctx context.Context) ([]db.Host, error)
	ListHostReplicaCounts(ctx context.Context) (map[uuid.UUID]int64, error)
	ListReplicas(ctx context.Context) ([]db.Replica, error)
	ListDeploymentReplicaIDs(ctx context.Context, deploymentID uuid.UUID) ([]uuid.UUID, error)

	ListProjectNames(ctx context.Context, project string) ([]string, error)
	ListRegionNames(ctx context.Context) ([]string, error)
	ListEnvironmentRows(ctx context.Context, project, environment string) ([]db.ListEnvironmentRowsRow, error)
	ListProjectServices(ctx context.Context, project, environment string) ([]db.ListProjectServicesRow, error)
	TopologyServices(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyServicesRow, error)
	TopologyDesiredRegions(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyDesiredRegionsRow, error)
	TopologyReplicas(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyReplicasRow, error)
	TopologyHosts(ctx context.Context, region string) ([]db.Host, error)
	TopologyServed(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyServedRow, error)
}

// HostOperator holds the operator transitions no agent can report: host
// scheduling state and the synthetic "row vanished" replica loss. The bool
// results mean "the host was in a state the transition applies to".
type HostOperator interface {
	CordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	UncordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DrainHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DeleteReplica(ctx context.Context, replicaID uuid.UUID) error
}

// OperatorAPIStore is what the OperatorAPI needs from storage;
// *storage.PostgresClient satisfies it.
type OperatorAPIStore interface {
	FleetReader
	HostOperator
}

// OperatorAPI is the operator-facing HTTP surface for the UI and CLI: fleet
// reads, the desired-state writes (create, deploy, scale) that go through the
// same project layer the CLI uses, and the operator transitions no agent can
// report (cordon, drain, synthetic row loss). Agent-observable chaos does NOT
// belong here — it goes through the agents (agentsim control API) so it travels
// the real transport.
type OperatorAPI struct {
	store   OperatorAPIStore
	desired DesiredState
}

func NewOperatorAPI(store OperatorAPIStore, desired DesiredState) *OperatorAPI {
	return &OperatorAPI{store: store, desired: desired}
}

func (o *OperatorAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, okJSON{OK: true}) })

	mux.HandleFunc("GET /v1/hosts", o.listHosts)
	mux.HandleFunc("POST /v1/hosts/{id}/cordon", o.hostTransition("cordon", o.store.CordonHost))
	mux.HandleFunc("POST /v1/hosts/{id}/uncordon", o.hostTransition("uncordon", o.store.UncordonHost))
	mux.HandleFunc("POST /v1/hosts/{id}/drain", o.hostTransition("drain", o.store.DrainHost))

	mux.HandleFunc("GET /v1/replicas", o.listReplicas)
	mux.HandleFunc("GET /v1/deployments/{id}/replicas", o.deploymentReplicas)
	mux.HandleFunc("DELETE /v1/replicas/{id}", o.deleteReplica)

	mux.HandleFunc("GET /v1/meta", o.meta)
	mux.HandleFunc("GET /v1/topology", o.topology)

	mux.HandleFunc("GET /v1/services", o.listServices)
	mux.HandleFunc("POST /v1/projects", o.createProject)
	mux.HandleFunc("POST /v1/environments", o.createEnvironment)
	mux.HandleFunc("POST /v1/services", o.createService)
	mux.HandleFunc("POST /v1/environment-services", o.bindService)
	mux.HandleFunc("POST /v1/deployments", o.deploy)
	mux.HandleFunc("POST /v1/deployments/scale", o.scale)
	return mux
}

// okJSON acknowledges a write that has nothing else to report.
type okJSON struct {
	OK bool `json:"ok"`
}

type errorJSON struct {
	Error string `json:"error"`
}

// filterValue reads a selector param. "all" is the UI's own word for "no
// filter" and is treated as absent, so a stale query string can't blank a view.
func filterValue(r *http.Request, name string) string {
	v := r.URL.Query().Get(name)
	if v == "all" {
		return ""
	}
	return v
}

// pathID parses the {id} path segment; a 400 is written on failure and the
// caller returns.
func pathID(w http.ResponseWriter, r *http.Request, what string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad %s id", what))
		return uuid.Nil, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is for 4xx: the message is the caller's to read and fix.
func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorJSON{Error: err.Error()})
}

// writeInternalError is for 5xx: the cause (driver text, SQL) is logged for the
// operator, never sent to the client.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("operatorapi: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, errorJSON{Error: "internal error"})
}

// Nullable SQL columns become JSON null so clients never see database/sql
// envelope types.

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func nullUUID(id uuid.NullUUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	return &id.UUID
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// orEmpty keeps a nil slice out of the JSON: clients iterate these unguarded,
// and `null` is not iterable.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
