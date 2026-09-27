package api

import (
	"fmt"

	"conductor/internal/config"
	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// placementBlockers explains, per unplaced replica, why the placer can't land
// it. The placer drops an assign_host it can't satisfy without writing a word,
// so the row alone reads "pending" forever; this replays the placer's own fit
// test (engine placer.fits) over the same hosts. No entry means some host fits
// and the next tick should place it.
func placementBlockers(reps []db.TopologyReplicasRow, hosts []db.Host, usage map[uuid.UUID]storage.HostUsage) map[uuid.UUID]string {
	headroom := config.DefaultPlacement().Headroom
	out := make(map[uuid.UUID]string)
	for _, r := range reps {
		// A volume-bound replica can only go to its volume's host — a region
		// scan would name hosts it could never use.
		if r.HostID.Valid || r.VolumeID.Valid || r.DesiredStatus != "running" {
			continue
		}
		if reason := placementBlocker(r, hosts, usage, headroom); reason != "" {
			out[r.ID] = reason
		}
	}
	return out
}

// optionalStr maps "" to JSON null, so a placed replica carries no blocker.
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func placementBlocker(r db.TopologyReplicasRow, hosts []db.Host, usage map[uuid.UUID]storage.HostUsage, headroom float64) string {
	// The headroom is held for replacements (a replica that lost its host),
	// so only a fresh replica is charged it.
	if domain.ReplicaPhase(r.Phase) == domain.ReplicaPhaseReplacing {
		headroom = 0
	}
	var inRegion, open int
	var best string
	var bestCPU, bestMem int64
	for _, h := range hosts {
		if h.Region != r.Region {
			continue
		}
		inRegion++
		if !h.HostHealthy || h.Status != string(domain.HostOpen) {
			continue
		}
		open++
		freeCPU := int64(h.CpuMillicores) - usage[h.ID].CPUMillicores - int64(headroom*float64(h.CpuMillicores))
		freeMem := h.MemBytes - usage[h.ID].MemBytes - int64(headroom*float64(h.MemBytes))
		if int64(r.CpuMillicores) <= freeCPU && r.MemBytes <= freeMem {
			return ""
		}
		if best == "" || freeCPU > bestCPU {
			best, bestCPU, bestMem = h.Hostname, freeCPU, freeMem
		}
	}
	switch {
	case inRegion == 0:
		return fmt.Sprintf("no hosts in %s", r.Region)
	case open == 0:
		return fmt.Sprintf("no healthy open host in %s (all cordoned, draining or down)", r.Region)
	}
	return fmt.Sprintf("no host in %s has room for %dm CPU / %d MiB; most free: %s with %dm / %d MiB",
		r.Region, r.CpuMillicores, r.MemBytes>>20, best, max(bestCPU, 0), max(bestMem, 0)>>20)
}
