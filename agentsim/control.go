package agentsim

import (
	"encoding/json"
	"net/http"
)

// The chaos control API. Same wire shape chaos-ui's /api/chaos already speaks
// ({action, id}-ish JSON), so the UI can eventually point here instead of
// editing the database behind the engine's back — chaos then flows through the
// real transport: agent lies → AgentAPI → ObservedState → SQL guards.

type chaosRequest struct {
	Action  string `json:"action"`
	Host    string `json:"host,omitempty"`
	Replica string `json:"replica,omitempty"`
	Volume  string `json:"volume,omitempty"`
}

func (f *Fleet) controlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", f.handleAgents)
	mux.HandleFunc("POST /chaos", f.handleChaos)
	return mux
}

func (f *Fleet) handleAgents(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.statuses())
}

func (f *Fleet) handleChaos(w http.ResponseWriter, r *http.Request) {
	var req chaosRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json")
		return
	}

	switch req.Action {
	case "host_kill", "host_recover":
		a := f.agentByHost(req.Host)
		if a == nil {
			httpError(w, http.StatusNotFound, "no agent for host "+req.Host)
			return
		}
		if req.Action == "host_kill" {
			a.KillHost()
		} else {
			a.RecoverHost()
		}

	case "replica_crash", "replica_crashloop", "replica_stall_health", "replica_heal":
		a := f.agentByReplica(req.Replica)
		if a == nil {
			httpError(w, http.StatusNotFound, "no agent runs replica "+req.Replica)
			return
		}
		ok := false
		switch req.Action {
		case "replica_crash":
			ok = a.CrashReplica(req.Replica)
		case "replica_crashloop":
			ok = a.CrashLoop(req.Replica)
		case "replica_stall_health":
			ok = a.StallHealth(req.Replica)
		case "replica_heal":
			ok = a.Heal(req.Replica)
		}
		if !ok {
			httpError(w, http.StatusNotFound, "replica "+req.Replica+" not on agent anymore")
			return
		}

	case "volume_stall_resize", "volume_heal":
		a := f.agentByVolume(req.Volume)
		if a == nil {
			httpError(w, http.StatusNotFound, "no agent holds volume "+req.Volume)
			return
		}
		ok := false
		switch req.Action {
		case "volume_stall_resize":
			ok = a.StallResize(req.Volume)
		case "volume_heal":
			ok = a.HealVolume(req.Volume)
		}
		if !ok {
			httpError(w, http.StatusNotFound, "volume "+req.Volume+" not on agent anymore")
			return
		}

	default:
		httpError(w, http.StatusBadRequest, "unknown action "+req.Action)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
