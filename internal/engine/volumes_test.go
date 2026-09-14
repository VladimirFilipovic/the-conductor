package engine

// Resize pass tests: pure table tests on (snapshot, ledger) → intents. The
// host is 10 GiB with flatPlacement's 0.8 budget / 0.2 reserve: 8 GiB packable,
// 6.4 GiB open to new placements, all 8 open to grows.

import (
	"reflect"
	"testing"

	"conductor/internal/domain"

	"github.com/google/uuid"
)

const gib = int64(1) << 30

func placedVolume(n byte, hostID uuid.UUID, status domain.VolumeStatus, desired, observed int64) volume {
	return volume{ID: pinnedID(n), ServiceID: pinnedID(100 + n), Region: "eu", HostID: hostID,
		DesiredSizeBytes: desired, ObservedSizeBytes: observed, Status: status}
}

func TestResizeVolumesStateMachine(t *testing.T) {
	h := testHost(1, "eu", 2000, 1<<30, 10*gib)
	other := placedVolume(9, h.ID, domain.VolumeAttached, 5*gib, 5*gib)

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
			name:    "beyond the budget holds — no failed, drift stays",
			volumes: []volume{placedVolume(2, h.ID, domain.VolumeAttached, 9*gib, 2*gib)},
			want:    nil,
		},
		{
			name:    "another volume's desired counts against the room",
			volumes: []volume{other, placedVolume(2, h.ID, domain.VolumeAttached, 4*gib, 1*gib)},
			want:    nil,
		},
		{
			name:    "never-observed volume has no drift to approve",
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
			name:    "volume on a host outside the ledger holds",
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
	fresh := volume{ID: pinnedID(3), ServiceID: pinnedID(103), Region: "eu", DesiredSizeBytes: 2 * gib, Status: domain.VolumePending}
	snap := stateSnapshot{hosts: []host{h}, volumes: []volume{growing, fresh}}
	p := placer{cfg: flatPlacement()}

	got := p.planVolumes(snap)

	// 8 GiB budget − 4 (growing's desired) = 4 left; 2 ≤ 4 − 1.6 reserve, so
	// the placement lands, and 4 − 2 ≥ 0 keeps the grow approved.
	want := []Intent{
		{Kind: IntentPlaceVolume, VolumeID: fresh.ID, HostID: h.ID},
		{Kind: IntentResizeVolume, VolumeID: growing.ID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planVolumes() = %v, want %v", got, want)
	}
}
