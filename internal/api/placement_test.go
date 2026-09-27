package api

import (
	"testing"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

func TestPlacementBlockers(t *testing.T) {
	host := db.Host{
		ID: uuid.New(), Region: "eu-west-1", Hostname: "ew1-small-1",
		CpuMillicores: 2000, MemBytes: 1 << 30, HostHealthy: true, Status: "open",
	}
	cordoned := host
	cordoned.ID, cordoned.Status = uuid.New(), "cordoned"
	unplaced := func(phase string, cpu int32) db.TopologyReplicasRow {
		return db.TopologyReplicasRow{
			ID: uuid.New(), Region: "eu-west-1", Phase: phase, DesiredStatus: "running",
			CpuMillicores: cpu, MemBytes: 256 << 20,
		}
	}

	tests := []struct {
		name  string
		rep   db.TopologyReplicasRow
		hosts []db.Host
		used  int64 // cpu millicores already reserved on host
		want  string
	}{
		{
			name: "a host fits: no blocker, the next tick places it",
			rep:  unplaced("pending", 500), hosts: []db.Host{host}, used: 1000,
		},
		{
			name: "full host names the shortfall and the closest candidate",
			rep:  unplaced("pending", 1200), hosts: []db.Host{host}, used: 1600,
			want: "no host in eu-west-1 has room for 1200m CPU / 256 MiB; most free: ew1-small-1 with 200m / 821 MiB",
		},
		{
			name: "the headroom is charged to a fresh replica",
			rep:  unplaced("pending", 300), hosts: []db.Host{host}, used: 1800,
			want: "no host in eu-west-1 has room for 300m CPU / 256 MiB; most free: ew1-small-1 with 0m / 821 MiB",
		},
		{
			name: "but not to a replacement, which may use it",
			rep:  unplaced("replacing", 150), hosts: []db.Host{host}, used: 1800,
		},
		{
			name: "no host in the region at all",
			rep:  unplaced("pending", 100),
			want: "no hosts in eu-west-1",
		},
		{
			name: "only cordoned hosts",
			rep:  unplaced("pending", 100), hosts: []db.Host{cordoned},
			want: "no healthy open host in eu-west-1 (all cordoned, draining or down)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := map[uuid.UUID]storage.HostUsage{host.ID: {CPUMillicores: tt.used, MemBytes: 100 << 20}}
			got := placementBlockers([]db.TopologyReplicasRow{tt.rep}, tt.hosts, usage)[tt.rep.ID]
			if got != tt.want {
				t.Errorf("blocker = %q, want %q", got, tt.want)
			}
		})
	}
}

// Placed and volume-bound replicas never get a blocker: one has a host, the
// other can only go to its volume's host, which a region scan can't judge.
func TestPlacementBlockersSkipsPlacedAndVolumeBound(t *testing.T) {
	placed := db.TopologyReplicasRow{ID: uuid.New(), Region: "eu-west-1", DesiredStatus: "running",
		HostID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, CpuMillicores: 100}
	bound := db.TopologyReplicasRow{ID: uuid.New(), Region: "eu-west-1", DesiredStatus: "running",
		VolumeID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, CpuMillicores: 100}

	if got := placementBlockers([]db.TopologyReplicasRow{placed, bound}, nil, nil); len(got) != 0 {
		t.Errorf("blockers = %v, want none", got)
	}
}
