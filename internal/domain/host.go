package domain

// HostStatus is the operator's intent for a host — one owner, one column.
// Liveness is a separate observed column (hosts.host_healthy) the heartbeat
// and the watchdog own, so a heartbeat can never overwrite a cordon and a
// silent drain stays a drain. Schedulable for free placement is
// host_healthy AND HostOpen. String-underlying to map onto the text column;
// keep in lockstep with the CHECK on hosts.status.
type HostStatus string

const (
	HostOpen     HostStatus = "open"     // accepting placements
	HostCordoned HostStatus = "cordoned" // no new placements; existing replicas stay
	HostDraining HostStatus = "draining" // evacuating stateless replicas, then cordoned
)

// Valid reports whether s is a known status — guard before writing one back.
func (s HostStatus) Valid() bool {
	switch s {
	case HostOpen, HostCordoned, HostDraining:
		return true
	}
	return false
}
