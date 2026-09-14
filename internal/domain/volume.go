package domain

import "time"

// VolumeLeaseTTL bounds how long a dead writer can hold a volume hostage. It is
// shared by the two sides of the lease: the engine acquires with it on
// placement, the apiserver's ObservedState renews with it on every healthy observation that
// lands — so a lease only outlives its holder by one TTL, never longer.
const VolumeLeaseTTL = 90 * time.Second

// ApiserverLivenessWindow is how long an apiserver may go without refreshing its
// apiserver_instances heartbeat before the engine stops counting it as live. The
// apiserver refreshes far more often than this; the window only has to absorb
// a stalled tick or a slow database, never a real outage.
const ApiserverLivenessWindow = 30 * time.Second

// VolumeStatus is a volume's lifecycle status. String-underlying to map onto
// the text column; keep in lockstep with the CHECK on volumes.status.
//
// Only the engine writes it: pending → attached on placement, attached ↔
// resizing around a grow. The agent never sees the status directly — it sees
// the size the control plane wants it to have, and resizing is what unlocks
// the new desired size on the downlink (see api.volumeTargetSize).
type VolumeStatus string

const (
	VolumePending  VolumeStatus = "pending"  // no host yet
	VolumeAttached VolumeStatus = "attached" // placed; observed size may lag desired (drift waits for host space)
	VolumeDetached VolumeStatus = "detached" // reserved: not reached by any path today
	VolumeResizing VolumeStatus = "resizing" // engine approved a grow; the agent is converging on desired_size_bytes
	VolumeFailed   VolumeStatus = "failed"   // reserved: a resize never gives up, it waits
)
