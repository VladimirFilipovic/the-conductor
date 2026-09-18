package engine

// Resize pass tests: pure table tests on (snapshot, ledger) → intents. The
// host is 10 GiB with flatPlacement's 0.8 budget / 0.2 reserve: 8 GiB packable,
// 6.4 GiB open to new placements, all 8 open to grows. The ledger charges
// Committed() bytes — what's on disk, or desired once approved (resizing) or
// never reported — so a requested grow is judged by its DELTA.

import (
	"reflect"
	"testing"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

const gib = int64(1) << 30

func placedVolume(n byte, hostID uuid.UUID, status domain.VolumeStatus, desired, observed int64) volume {
	return volume{ID: pinnedID(n), ServiceID: pinnedID(100 + n), Region: "eu", HostID: hostID,
		VolumeSizing: domain.VolumeSizing{Status: status, Desired: desired, Observed: observed}}
}

func TestResizeVolumesStateMachine(t *testing.T) {
	h := testHost(1, "eu", 2000, 1<<30, 10*gib)

	tests := []struct {
		name    string
		volumes []volume
		want    []Intent
	}{
		{
			name:    "attached with drift and room approves the grow",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 6*gib, 2*gib)},
			want:    []Intent{{Kind: IntentResizeVolume, VolumeID: pinnedID(2)}},
		},
		{
			name:    "a grow may fill the whole budget, reserve included",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 8*gib, 2*gib)},
			want:    []Intent{{Kind: IntentResizeVolume, VolumeID: pinnedID(2)}},
		},
		{
			name:    "beyond the budget parks the grow as resize_pending — no failed",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 9*gib, 2*gib)},
			want:    []Intent{{Kind: IntentVolumeResizePending, VolumeID: pinnedID(2)}},
		},
		{
			// 8 − 5 (other, on disk) − 1 (own, on disk) = 2 < delta 3.
			name: "another volume's on-disk bytes count against the room",
			volumes: []volume{
				placedVolume(9, h.ID, domain.VolumeAttached, 5*gib, 5*gib),
				placedVolume(2, h.ID, domain.VolumeAttached, 4*gib, 1*gib),
			},
			want: []Intent{{Kind: IntentVolumeResizePending, VolumeID: pinnedID(2)}},
		},
		{
			// The parked 100 GiB request charges only its 2 GiB on disk:
			// 8 − 2 − 2 = 4 ≥ delta 4, so the second grow is approved. Charging
			// desired would have made the host look 94 GiB overcommitted.
			name: "an unapproved grow is not a reservation",
			volumes: []volume{
				placedVolume(9, h.ID, domain.VolumeResizePending, 100*gib, 2*gib),
				placedVolume(2, h.ID, domain.VolumeAttached, 6*gib, 2*gib),
			},
			want: []Intent{{Kind: IntentResizeVolume, VolumeID: pinnedID(2)}},
		},
		{
			// An approved grow charges desired: 8 − 6 (resizing, still at 2 on
			// disk) − 2 = 0 < delta 3.
			name: "a resizing volume charges its desired size",
			volumes: []volume{
				placedVolume(9, h.ID, domain.VolumeResizing, 6*gib, 2*gib),
				placedVolume(2, h.ID, domain.VolumeAttached, 5*gib, 2*gib),
			},
			want: []Intent{{Kind: IntentVolumeResizePending, VolumeID: pinnedID(2)}},
		},
		{
			// Room for one delta of 4 (8 − 2 − 2): the first in snapshot order
			// commits it, the second sees the reservation made this tick.
			name: "two grows on one host, room for one — first by ID wins",
			volumes: []volume{
				placedVolume(2, h.ID, domain.VolumeAttached, 6*gib, 2*gib),
				placedVolume(3, h.ID, domain.VolumeAttached, 6*gib, 2*gib),
			},
			want: []Intent{
				{Kind: IntentResizeVolume, VolumeID: pinnedID(2)},
				{Kind: IntentVolumeResizePending, VolumeID: pinnedID(3)},
			},
		},
		{
			name:    "resize_pending with room is approved",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeResizePending, 6*gib, 2*gib)},
			want:    []Intent{{Kind: IntentResizeVolume, VolumeID: pinnedID(2)}},
		},
		{
			name:    "resize_pending without room stays put — no second intent",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeResizePending, 9*gib, 2*gib)},
			want:    nil,
		},
		{
			name:    "resize_pending and caught up (a revert) settles",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeResizePending, 2*gib, 2*gib)},
			want:    []Intent{{Kind: IntentVolumeResized, VolumeID: pinnedID(2)}},
		},
		{
			name:    "never-observed volume has no drift to classify",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 6*gib, 0)},
			want:    nil,
		},
		{
			name:    "converged volume is left alone",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 6*gib, 6*gib)},
			want:    nil,
		},
		{
			name:    "resizing and caught up settles",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeResizing, 6*gib, 6*gib)},
			want:    []Intent{{Kind: IntentVolumeResized, VolumeID: pinnedID(2)}},
		},
		{
			name:    "resizing but still growing waits on the agent",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeResizing, 6*gib, 4*gib)},
			want:    nil,
		},
		{
			name:    "volume on a host outside the ledger holds, status untouched",
			volumes: []volume{placedVolume(2, pinnedID(7), domain.VolumeAttached, 6*gib, 2*gib)},
			want:    nil,
		},
		{
			name:    "pending (hostless) volume is not a resize",
			volumes: []volume{placedVolume(2, uuid.Nil, domain.VolumePending, 6*gib, 0)},
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := stateSnapshot{hosts: []host{h}, volumes: tt.volumes}
			p := placer{cfg: flatPlacement()}
			got := p.resizeVolumes(snap, p.newLedger(snap))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resizeVolumes() = %v, want %v", got, tt.want)
			}
		})
	}
}

// planVolumes runs placement and resize off one ledger in one tick: a fresh
// volume lands and a drifting one is approved together, each seeing the
// other's claim on the host.
func TestPlanVolumesPlacesAndGrowsInOneTick(t *testing.T) {
	h := testHost(1, "eu", 2000, 1<<30, 10*gib)
	growing := placedVolume(2, h.ID, domain.VolumeAttached, 4*gib, 2*gib)
	fresh := volume{ID: pinnedID(3), ServiceID: pinnedID(103), Region: "eu",
		VolumeSizing: domain.VolumeSizing{Status: domain.VolumePending, Desired: 2 * gib}}
	snap := stateSnapshot{hosts: []host{h}, volumes: []volume{growing, fresh}}
	p := placer{cfg: flatPlacement()}

	got := p.planVolumes(snap)

	// 8 GiB budget − 2 (growing, on disk) = 6 left; 2 ≤ 6 − 1.6 reserve, so
	// the placement lands, and delta 2 ≤ 6 − 2 keeps the grow approved.
	want := []Intent{
		{Kind: IntentPlaceVolume, VolumeID: fresh.ID, HostID: h.ID},
		{Kind: IntentResizeVolume, VolumeID: growing.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planVolumes() = %v, want %v", got, want)
	}
}

// The point of charging committed rather than desired bytes: a grow the host
// cannot hold is parked, and the host stays open to new volumes meanwhile. An
// 80 GiB host (64 GiB budget, 51.2 open to placements) holding a 4 GiB disk
// with a 100 GiB request still takes a fresh 30 GiB volume.
func TestPlanVolumesUnapprovedGrowDoesNotBlockPlacement(t *testing.T) {
	h := testHost(1, "eu", 2000, 1<<30, 80*gib)
	wanting := placedVolume(2, h.ID, domain.VolumeAttached, 100*gib, 4*gib)
	fresh := volume{ID: pinnedID(3), ServiceID: pinnedID(103), Region: "eu",
		VolumeSizing: domain.VolumeSizing{Status: domain.VolumePending, Desired: 30 * gib}}
	snap := stateSnapshot{hosts: []host{h}, volumes: []volume{wanting, fresh}}
	p := placer{cfg: flatPlacement()}

	got := p.planVolumes(snap)

	want := []Intent{
		{Kind: IntentPlaceVolume, VolumeID: fresh.ID, HostID: h.ID},
		{Kind: IntentVolumeResizePending, VolumeID: wanting.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planVolumes() = %v, want %v", got, want)
	}
}
