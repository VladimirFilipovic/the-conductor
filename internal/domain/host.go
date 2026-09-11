package domain

// HostStatus is a host's scheduling status. String-underlying to map onto the
// text column; keep in lockstep with the CHECK on hosts.status.
type HostStatus string

const (
	HostReady    HostStatus = "ready"    // accepting placements
	HostNotReady HostStatus = "notready" // alive but not schedulable (agent-reported or swept stale)
	HostDraining HostStatus = "draining" // operator: evacuate replicas, then cordon
	HostCordoned HostStatus = "cordoned" // operator: no new placements
)

// Valid reports whether s is a known status — guard before writing one back.
func (s HostStatus) Valid() bool {
	switch s {
	case HostReady, HostNotReady, HostDraining, HostCordoned:
		return true
	}
	return false
}

// AgentReportable reports whether a heartbeat may carry this status. Draining
// and cordoned are operator-owned desired state: a heartbeat must never be able
// to lift a cordon, and an unknown value would trip the CHECK on every beat
// until the live host is falsely swept as stale.
func (s HostStatus) AgentReportable() bool {
	return s == HostReady || s == HostNotReady
}
