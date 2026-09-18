package engine

import (
	"log/slog"

	"conductor/internal/domain"
)

// planVolumes is the placer's volume pass: place hostless volumes, then
// decide grows against the SAME ledger, so a volume landing this tick already
// counts against the host when the grow is judged.
func (p *placer) planVolumes(snap stateSnapshot) []Intent {
	led := p.newLedger(snap)
	intents := p.placeVolumes(snap, led)
	return append(intents, p.resizeVolumes(snap, led)...)
}

// resizeVolumes drives the grow-only resize state machine, one volume at a
// time, against the pass's ledger:
//
//	attached,       drifting, delta fits   → resize_volume          (→ resizing)
//	attached,       drifting, no room      → volume_resize_pending  (→ resize_pending)
//	resize_pending, drifting, delta fits   → resize_volume          (→ resizing)
//	resize_pending, caught up              → volume_resized         (→ attached; a revert)
//	resizing,       caught up              → volume_resized         (→ attached)
//
// "Fits" is fits with a resize item whose disk is the grow DELTA: the ledger
// already charges Committed() (what's on disk, or desired once approved), so
// the delta is exactly what the grow still has to find, and a resize item
// may take the DiskReserve — see packItem.resize. An approved grow charges
// its delta onto the ledger at once, so a second grow on the same host this
// tick is judged against it. An unhealthy host is outside the ledger, so a
// grow there cannot fit and is parked like any other — that keeps `volume
// revert` available while the host is down, and the settle branch needs no
// ledger, so a revert lands as attached on the next tick regardless. A
// cordoned or draining host is in the ledger and grows normally — the volume
// is staying on it regardless.
//
// There is no failed exit. resize_pending is the only outcome for a grow
// that doesn't fit: the pass re-evaluates it every tick until the host has
// room, and `volume revert` is the operator's way out.
//
// A never-reported volume has nothing to classify: the downlink hands the
// agent desired as its first size.
func (p *placer) resizeVolumes(snap stateSnapshot, led ledger) []Intent {
	var intents []Intent
	for _, v := range snap.volumes {
		if !v.Reported() {
			continue
		}
		switch {
		case v.Status == domain.VolumeResizing && v.CaughtUp(),
			v.Status == domain.VolumeResizePending && v.CaughtUp():
			intents = append(intents, Intent{Kind: IntentVolumeResized, VolumeID: v.ID})
			slog.Info("reconcile -> volume resized",
				"volume", v.ID, "host", v.HostID, "from", v.Status, "bytes", v.Observed)

		case v.Status == domain.VolumeAttached && v.Drifting(),
			v.Status == domain.VolumeResizePending && v.Drifting():
			// hl is nil for an unhealthy host: nothing fits there, so the grow
			// parks and the operator keeps the revert exit.
			hl := led[v.HostID]
			item := packItem{id: v.ID, region: v.Region, disk: v.GrowDelta(), resize: true}
			if hl != nil && p.fits(item, hl) {
				led.commit(item, v.HostID)
				intents = append(intents, Intent{Kind: IntentResizeVolume, VolumeID: v.ID})
				slog.Info("reconcile -> volume grow approved",
					"volume", v.ID, "host", v.HostID, "desired", v.Desired, "observed", v.Observed)
				continue
			}
			if v.Status == domain.VolumeResizePending {
				slog.Debug("reconcile -> volume grow still waiting for host space",
					"volume", v.ID, "host", v.HostID, "desired", v.Desired, "observed", v.Observed)
				continue
			}
			intents = append(intents, Intent{Kind: IntentVolumeResizePending, VolumeID: v.ID})
			slog.Info("reconcile -> volume grow waiting for host space",
				"volume", v.ID, "host", v.HostID, "desired", v.Desired, "observed", v.Observed)
		}
	}
	return intents
}
