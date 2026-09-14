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
// time, no coupling between volumes:
//
//	attached, desired > observed, host has room → resize_volume  (→ resizing)
//	attached, desired > observed, no room       → hold; drift stays visible, retried every tick
//	resizing, observed >= desired               → volume_resized (→ attached)
//
// "Has room" is fits with a resize item: newLedger already subtracts every
// placed volume's DESIRED size, so a bumped desired is a reservation the
// moment it lands, and the grow fits iff the host isn't overcommitted (the
// DiskReserve is its to use — see packItem.resize). A host outside the ledger
// (notready, cordoned) holds too: no new work lands on it, a grow included.
//
// There is no failed exit. Waiting is the only outcome for a grow that doesn't
// fit: the operator sees desired > observed on an attached volume, and the
// pass re-evaluates on every tick until the host has room.
//
// A never-observed volume (ObservedSizeBytes 0) doesn't drift: the downlink
// hands the agent desired as its first size, so there is nothing to approve.
func (p *placer) resizeVolumes(snap stateSnapshot, led ledger) []Intent {
	var intents []Intent
	for _, v := range snap.volumes {
		switch {
		case v.Status == domain.VolumeResizing && v.ObservedSizeBytes >= v.DesiredSizeBytes:
			intents = append(intents, Intent{Kind: IntentVolumeResized, VolumeID: v.ID})
			slog.Info("reconcile -> volume resized",
				"volume", v.ID, "host", v.HostID, "bytes", v.ObservedSizeBytes)

		case v.Status == domain.VolumeAttached && v.ObservedSizeBytes > 0 && v.DesiredSizeBytes > v.ObservedSizeBytes:
			hl := led[v.HostID]
			if hl == nil || !p.fits(packItem{id: v.ID, region: v.Region, resize: true}, hl) {
				slog.Debug("reconcile -> volume grow waiting for host space",
					"volume", v.ID, "host", v.HostID,
					"desired", v.DesiredSizeBytes, "observed", v.ObservedSizeBytes)
				continue
			}
			intents = append(intents, Intent{Kind: IntentResizeVolume, VolumeID: v.ID})
			slog.Info("reconcile -> volume grow approved",
				"volume", v.ID, "host", v.HostID,
				"desired", v.DesiredSizeBytes, "observed", v.ObservedSizeBytes)
		}
	}
	return intents
}
