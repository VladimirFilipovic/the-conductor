package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// maxRequestBytes caps a control-plane request body. Every write here is a
// handful of scalars, so anything larger is a mistake or an attack.
const maxRequestBytes = 1 << 20

// ControlPlaneStore is what the read/operator HTTP API needs from storage.
// Consumer-side view; *storage.PostgresClient satisfies it.
type ControlPlaneStore interface {
	ListAgentHosts(ctx context.Context) ([]db.Host, error)
	ListReplicas(ctx context.Context) ([]db.Replica, error)

	// Dashboard reads: the naming tree, the fleet, and the desired-vs-observed
	// slices the topology view joins together.
	ListProjectNames(ctx context.Context, project string) ([]string, error)
	ListRegionNames(ctx context.Context) ([]string, error)
	ListEnvironmentRows(ctx context.Context, project, environment string) ([]db.ListEnvironmentRowsRow, error)
	ListProjectServices(ctx context.Context, project, environment string) ([]db.ListProjectServicesRow, error)
	TopologyServices(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyServicesRow, error)
	TopologyDesiredRegions(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyDesiredRegionsRow, error)
	TopologyReplicas(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyReplicasRow, error)
	TopologyHosts(ctx context.Context, region string) ([]db.Host, error)
	TopologyServed(ctx context.Context, f storage.TopologyFilter) ([]db.TopologyServedRow, error)

	// Fan-out targets for deployment-wide agent chaos.
	ListDeploymentReplicaIDs(ctx context.Context, deploymentID uuid.UUID) ([]uuid.UUID, error)

	// Operator host transitions; false = the host was not in a state the
	// transition applies to (or does not exist).
	CordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	UncordonHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DrainHost(ctx context.Context, hostID uuid.UUID) (bool, error)
	DeleteReplica(ctx context.Context, replicaID uuid.UUID) error
}

// ControlPlane is the operator-facing HTTP surface: fleet reads, the
// desired-state writes the CLI also makes (create, deploy, scale), and the
// operator transitions no agent can report (cordon, drain, synthetic row loss).
// Agent-observable chaos does NOT belong here — it goes through the agents
// (agentsim control API) so it travels the real transport.
type ControlPlane struct {
	store   ControlPlaneStore
	desired DesiredState
}

func NewControlPlane(store ControlPlaneStore, desired DesiredState) *ControlPlane {
	return &ControlPlane{store: store, desired: desired}
}

func (c *ControlPlane) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /v1/hosts", c.listHosts)
	mux.HandleFunc("GET /v1/replicas", c.listReplicas)
	mux.HandleFunc("GET /v1/meta", c.meta)
	mux.HandleFunc("GET /v1/topology", c.topology)
	mux.HandleFunc("GET /v1/services", c.listServices)
	mux.HandleFunc("GET /v1/deployments/{id}/replicas", c.deploymentReplicas)
	mux.HandleFunc("POST /v1/projects", c.createProject)
	mux.HandleFunc("POST /v1/environments", c.createEnvironment)
	mux.HandleFunc("POST /v1/services", c.createService)
	mux.HandleFunc("POST /v1/environment-services", c.bindService)
	mux.HandleFunc("POST /v1/deployments", c.deploy)
	mux.HandleFunc("POST /v1/deployments/scale", c.scale)
	mux.HandleFunc("POST /v1/hosts/{id}/cordon", c.hostTransition("cordon", c.store.CordonHost))
	mux.HandleFunc("POST /v1/hosts/{id}/uncordon", c.hostTransition("uncordon", c.store.UncordonHost))
	mux.HandleFunc("POST /v1/hosts/{id}/drain", c.hostTransition("drain", c.store.DrainHost))
	mux.HandleFunc("DELETE /v1/replicas/{id}", c.deleteReplica)
	return mux
}

// Wire shapes: nullable SQL columns become omitted/zero JSON fields so clients
// never see database/sql envelope types.
type hostJSON struct {
	ID            uuid.UUID  `json:"id"`
	Hostname      string     `json:"hostname"`
	Region        string     `json:"region"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	CPUMillicores int32      `json:"cpu_millicores"`
	MemBytes      int64      `json:"mem_bytes"`
	DiskBytes     int64      `json:"disk_bytes"`
}

type replicaJSON struct {
	ID             uuid.UUID  `json:"id"`
	DeploymentID   uuid.UUID  `json:"deployment_id"`
	Region         string     `json:"region"`
	HostID         *uuid.UUID `json:"host_id,omitempty"`
	VolumeID       *uuid.UUID `json:"volume_id,omitempty"`
	DesiredStatus  string     `json:"desired_status"`
	Phase          string     `json:"phase"`
	Healthy        bool       `json:"healthy"`
	RestartCount   int32      `json:"restart_count"`
	LastExitReason string     `json:"last_exit_reason,omitempty"`
	Revision       int64      `json:"revision"`
	DrainedAt      *time.Time `json:"drained_at,omitempty"`
}

func (c *ControlPlane) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := c.store.ListAgentHosts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, hostsJSON(hosts))
}

func hostsJSON(hosts []db.Host) []hostJSON {
	out := make([]hostJSON, len(hosts))
	for i, h := range hosts {
		out[i] = hostJSON{
			ID: h.ID, Hostname: h.Hostname, Region: h.Region, Status: h.Status,
			LastHeartbeat: nullTime(h.LastHeartbeat.Time, h.LastHeartbeat.Valid),
			CPUMillicores: h.CpuMillicores, MemBytes: h.MemBytes, DiskBytes: h.DiskBytes,
		}
	}
	return out
}

func (c *ControlPlane) listReplicas(w http.ResponseWriter, r *http.Request) {
	replicas, err := c.store.ListReplicas(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]replicaJSON, len(replicas))
	for i, rep := range replicas {
		out[i] = replicaJSON{
			ID: rep.ID, DeploymentID: rep.DeploymentID, Region: rep.Region,
			HostID: nullUUID(rep.HostID), VolumeID: nullUUID(rep.VolumeID),
			DesiredStatus: rep.DesiredStatus, Phase: rep.Phase, Healthy: rep.Healthy,
			RestartCount: rep.RestartCount, LastExitReason: rep.LastExitReason.String,
			Revision: rep.Revision, DrainedAt: nullTime(rep.DrainedAt.Time, rep.DrainedAt.Valid),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hostTransition adapts one operator write: 404 for a bad id, 409 when the
// host is not in a state the transition applies to.
func (c *ControlPlane) hostTransition(name string, fn func(context.Context, uuid.UUID) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("bad host id"))
			return
		}
		ok, err := fn(r.Context(), hostID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusConflict, errors.New(name+": host not in an applicable state"))
			return
		}
		slog.Info("controlplane -> host "+name, "host", hostID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// deleteReplica is the synthetic "row vanished" failure: the reconciler sees
// the deployment short one replica and ramps a replacement. Unconditional on
// purpose — that is the point of the chaos.
func (c *ControlPlane) deleteReplica(w http.ResponseWriter, r *http.Request) {
	replicaID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad replica id"))
		return
	}
	if err := c.store.DeleteReplica(r.Context(), replicaID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	slog.Info("controlplane -> replica deleted", "replica", replicaID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func nullTime(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}

func nullUUID(id uuid.NullUUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	return &id.UUID
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
