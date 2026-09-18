package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

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

// replicaIDsJSON lists the fan-out targets for deployment-wide agent chaos.
type replicaIDsJSON struct {
	ReplicaIDs []uuid.UUID `json:"replica_ids"`
}

func replicasJSON(replicas []db.Replica) []replicaJSON {
	out := make([]replicaJSON, len(replicas))
	for i, rep := range replicas {
		out[i] = replicaJSON{
			ID: rep.ID, DeploymentID: rep.DeploymentID, Region: rep.Region,
			HostID: nullUUID(rep.HostID), VolumeID: nullUUID(rep.VolumeID),
			DesiredStatus: rep.DesiredStatus, Phase: rep.Phase, Healthy: rep.Healthy,
			RestartCount: rep.RestartCount, LastExitReason: rep.LastExitReason.String,
			Revision: rep.Revision, DrainedAt: nullTime(rep.DrainedAt),
		}
	}
	return out
}

func (o *OperatorAPI) listReplicas(w http.ResponseWriter, r *http.Request) {
	replicas, err := o.store.ListReplicas(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, replicasJSON(replicas))
}

func (o *OperatorAPI) deploymentReplicas(w http.ResponseWriter, r *http.Request) {
	deploymentID, ok := pathID(w, r, "deployment")
	if !ok {
		return
	}
	ids, err := o.store.ListDeploymentReplicaIDs(r.Context(), deploymentID)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, replicaIDsJSON{ReplicaIDs: orEmpty(ids)})
}

// deleteReplica is the synthetic "row vanished" failure: the reconciler sees
// the deployment short one replica and ramps a replacement. Unconditional on
// purpose — that is the point of the chaos.
func (o *OperatorAPI) deleteReplica(w http.ResponseWriter, r *http.Request) {
	replicaID, ok := pathID(w, r, "replica")
	if !ok {
		return
	}
	if err := o.store.DeleteReplica(r.Context(), replicaID); err != nil {
		writeInternalError(w, r, err)
		return
	}
	slog.Info("operatorapi -> replica deleted", "replica", replicaID)
	writeJSON(w, http.StatusOK, okJSON{OK: true})
}

// restartReplica thaws a frozen (failed) replica: the row goes hostless
// 'replacing' with a fresh restart budget and the reconciler re-places it
// next tick — the same route a host death takes. Only failed replicas
// qualify; a live one is 409, since restarting it is the agent's business,
// not the control plane's.
func (o *OperatorAPI) restartReplica(w http.ResponseWriter, r *http.Request) {
	replicaID, ok := pathID(w, r, "replica")
	if !ok {
		return
	}
	err := o.store.RestartReplica(r.Context(), replicaID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("restart: no such replica"))
		return
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, errors.New("restart: replica is not failed"))
		return
	case err != nil:
		writeInternalError(w, r, err)
		return
	}
	slog.Info("operatorapi -> replica restart requested", "replica", replicaID)
	writeJSON(w, http.StatusOK, okJSON{OK: true})
}
