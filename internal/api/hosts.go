package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

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

func hostsJSON(hosts []db.Host) []hostJSON {
	out := make([]hostJSON, len(hosts))
	for i, h := range hosts {
		out[i] = hostJSON{
			ID: h.ID, Hostname: h.Hostname, Region: h.Region, Status: h.Status,
			LastHeartbeat: nullTime(h.LastHeartbeat),
			CPUMillicores: h.CpuMillicores, MemBytes: h.MemBytes, DiskBytes: h.DiskBytes,
		}
	}
	return out
}

func (o *OperatorAPI) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := o.store.ListAgentHosts(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, hostsJSON(hosts))
}

// hostTransition adapts one operator write: 400 for a bad id, 409 when the
// host is not in a state the transition applies to.
func (o *OperatorAPI) hostTransition(name string, fn func(context.Context, uuid.UUID) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostID, ok := pathID(w, r, "host")
		if !ok {
			return
		}
		applied, err := fn(r.Context(), hostID)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		if !applied {
			writeError(w, http.StatusConflict, errors.New(name+": host not in an applicable state"))
			return
		}
		slog.Info("operatorapi -> host transition", "transition", name, "host", hostID)
		writeJSON(w, http.StatusOK, okJSON{OK: true})
	}
}
