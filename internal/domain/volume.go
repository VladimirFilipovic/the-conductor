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
// Only the engine writes it: pending → attached on placement, then around a
// grow attached → resizing (room now) or attached → resize_pending (waiting
// for room) → resizing, and back to attached once the disk has caught up. The
// CLI only ever moves desired_size_bytes. The agent never sees the status
// directly — it sees the size the control plane wants it to have, and
// resizing is what unlocks the new desired size on the downlink (see
// VolumeSizing.Committed).
type VolumeStatus string

const (
	VolumePending  VolumeStatus = "pending"  // no host yet
	VolumeAttached VolumeStatus = "attached" // placed; desired may briefly exceed observed until the engine classifies the grow
	VolumeDetached VolumeStatus = "detached" // reserved: not reached by any path today
	// VolumeResizePending: a grow is requested but the host has no room for
	// the delta. Engine-written so the operator can tell "waiting for space"
	// from "the agent is growing it" without comparing two size columns, and
	// so `volume revert` has an unambiguous state to act on — the only state
	// where the request is not yet a promise to the agent.
	VolumeResizePending VolumeStatus = "resize_pending"
	VolumeResizing      VolumeStatus = "resizing" // engine approved a grow; the agent is converging on desired_size_bytes
	VolumeFailed        VolumeStatus = "failed"   // reserved: a resize never gives up, it waits
)

// VolumeSizing is the one home for what a volume's two sizes and status mean
// together. The engine's ledger, the agent downlink and the CLI's guards all
// derive from these predicates, so "is it drifting", "has it settled" and
// "how many bytes does the host owe this volume" cannot disagree between
// layers. The SQL Mark* predicates restate Drifting/CaughtUp on purpose — the
// commit-time belt has to live in the database.
type VolumeSizing struct {
	Status   VolumeStatus
	Desired  int64
	Observed int64 // 0 = the agent has never reported this volume
}

func (s VolumeSizing) Reported() bool { return s.Observed > 0 }

// Drifting: a grow is requested — the agent has reported and desired is above
// what is on disk. Mirrored by MarkVolumeResizing/MarkVolumeResizePending.
func (s VolumeSizing) Drifting() bool { return s.Reported() && s.Desired > s.Observed }

// CaughtUp is the settle condition: the disk holds at least desired. True
// after a completed grow and after a revert. Mirrored by MarkVolumeAttached.
func (s VolumeSizing) CaughtUp() bool { return s.Reported() && s.Observed >= s.Desired }

// GrowDelta is the bytes a grow still has to find on the host.
func (s VolumeSizing) GrowDelta() int64 { return s.Desired - s.Observed }

// Committed: the bytes the control plane stands behind — what the agent is
// told to have on disk AND what the ledger charges the host. Desired only
// once approved (resizing) or when nothing has ever been reported (first
// size); otherwise what is on disk, so an unapproved request never blocks
// other volumes from landing.
func (s VolumeSizing) Committed() int64 {
	if s.Status == VolumeResizing || !s.Reported() {
		return s.Desired
	}
	return s.Observed
}
