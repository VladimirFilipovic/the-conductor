// Package domain holds the shared value types of the control plane — the typed
// enums that mirror the schema's text CHECK constraints, so engine and storage
// reference one source of truth instead of scattering bare string literals.
package domain

// ReplicaPhase is the replica lifecycle phase (§7). String-underlying so it maps
// straight onto the text column and stays readable in the DB; keep the constants
// in lockstep with the CHECK on replicas.phase.
type ReplicaPhase string

const (
	ReplicaPhasePending     ReplicaPhase = "pending"      // created, not yet placed
	ReplicaPhaseScheduling  ReplicaPhase = "scheduling"   // bin-packing a host
	ReplicaPhaseStarting    ReplicaPhase = "starting"     // placed, container coming up
	ReplicaPhaseHealthCheck ReplicaPhase = "health_check" // up, awaiting first healthy probe
	ReplicaPhaseHealthy     ReplicaPhase = "healthy"      // passing health checks
	ReplicaPhaseShifting    ReplicaPhase = "shifting"     // being moved between hosts
	ReplicaPhaseActive      ReplicaPhase = "active"       // serving
	ReplicaPhaseDraining    ReplicaPhase = "draining"     // graceful shutdown in progress
	ReplicaPhaseReaped      ReplicaPhase = "reaped"       // terminal: gone, excluded from snapshots
	ReplicaPhaseFailed      ReplicaPhase = "failed"       // terminal: crashed past restart_max
	// ReplicaPhaseReplacing: lost its host (host death) and awaits re-placement.
	// Orchestrator-owned like draining — set by MarkHostDown, exited only by the
	// reconcile path (AssignReplicaHost → scheduling), never by an agent report:
	// a partitioned-but-alive agent must not resurrect a freed replica.
	ReplicaPhaseReplacing ReplicaPhase = "replacing"
)

// Valid reports whether p is a known phase — guard before writing one back.
func (p ReplicaPhase) Valid() bool {
	switch p {
	case ReplicaPhasePending, ReplicaPhaseScheduling, ReplicaPhaseStarting,
		ReplicaPhaseHealthCheck, ReplicaPhaseHealthy, ReplicaPhaseShifting,
		ReplicaPhaseActive, ReplicaPhaseDraining, ReplicaPhaseReaped, ReplicaPhaseFailed,
		ReplicaPhaseReplacing:
		return true
	}
	return false
}

// Terminal reports whether the phase is an end state the engine never advances
// out of — reaped and failed replicas are done, not rescheduled.
func (p ReplicaPhase) Terminal() bool {
	return p == ReplicaPhaseReaped || p == ReplicaPhaseFailed
}

// ReplicaDesiredStatus is the operator's intent for a replica (run vs stop),
// independent of where its lifecycle phase currently sits.
type ReplicaDesiredStatus string

const (
	ReplicaDesiredRunning ReplicaDesiredStatus = "running"
	ReplicaDesiredStopped ReplicaDesiredStatus = "stopped"
)
