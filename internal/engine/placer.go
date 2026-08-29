package engine

// placer is the WHERE half of the Reconciler: the rules cascade decides WHAT
// (create / drain / assign_host ...), the placer decides WHICH host.
// Constraint-filtered dot-product best-fit decreasing — see docs/bin-pack.md.
// Pure function of (snapshot, intents): no clock, no randomness, so placement
// stays table-testable and deterministic.

import (
	"bytes"
	"slices"

	"conductor/internal/config"
	"conductor/internal/domain"

	"github.com/google/uuid"
)

type placer struct {
	cfg config.Placement
}

// hostLedger is one host's in-pass residual capacity. Seeded from the snapshot
// and mutated per placement, so a batch can't overcommit one host N times and
// anti-affinity sees placements made this tick. Never pre-credits capacity
// freed by same-tick destroys — freed space becomes visible next snapshot
// (conservative: one tick of latency, overcommit impossible).
type hostLedger struct {
	host host
	cpu  int64
	mem  int64
	// disk is what's left of the volume budget (DiskBytes · volume-budget)
	// after every placed volume. The grow-only-resize reserve is subtracted at
	// fit time, not here, because resizes may dip into it while new volumes
	// may not — so disk can legitimately sit below the reserve line.
	disk  int64
	slots map[replicaSlot]int
}

type ledger map[uuid.UUID]*hostLedger

func (p *placer) diskBudget(h host) int64 {
	return int64(float64(h.DiskBytes) * p.cfg.VolumeBudget)
}

func (p *placer) newLedger(snap stateSnapshot) ledger {
	led := make(ledger, len(snap.hosts))
	for _, h := range snap.hosts {
		led[h.ID] = &hostLedger{
			host:  h,
			cpu:   int64(h.CPUMillicores),
			mem:   h.MemBytes,
			disk:  p.diskBudget(h),
			slots: map[replicaSlot]int{},
		}
	}
	for _, r := range snap.replicas {
		hl := led[r.HostID]
		if hl == nil {
			continue
		}
		// Failed replicas still hold their host reservation until reaped, so
		// they count against capacity; they just aren't "running" for
		// anti-affinity purposes.
		hl.cpu -= int64(r.CPUMillicores)
		hl.mem -= r.MemBytes
		if !r.Phase.Terminal() {
			hl.slots[r.Slot]++
		}
	}
	for _, v := range snap.volumes {
		if hl := led[v.HostID]; hl != nil {
			hl.disk -= v.DesiredSizeBytes
		}
	}
	return led
}

// schedulable reports whether hostID is a real, currently schedulable host —
// non-nil and present in this pass's ledger (down/cordoned hosts never enter it).
func (led ledger) schedulable(hostID uuid.UUID) bool {
	return hostID != uuid.Nil && led[hostID] != nil
}

func (led ledger) commit(it packItem, hostID uuid.UUID) {
	hl := led[hostID]
	hl.cpu -= it.cpu
	hl.mem -= it.mem
	hl.disk -= it.disk
	hl.slots[it.slot]++
}

// packItem is one placement demand — a hostless replica (cpu+mem) or a
// hostless volume (cpu+mem gate + disk).
type packItem struct {
	id     uuid.UUID
	slot   replicaSlot
	region string
	cpu    int64
	mem    int64
	disk   int64
	// replacement (hostless after host death) skips the headroom reserve —
	// the reserve exists exactly for them.
	replacement bool
	// spread applies the anti-affinity constraint; volumes never spread
	// (stateful services run one replica, nothing to spread).
	spread bool
}

type weights struct{ cpu, mem, disk float64 }

type regionWeights map[string]weights

func (rw regionWeights) get(region string) weights {
	if w, ok := rw[region]; ok {
		return w
	}
	return weights{cpu: 1, mem: 1, disk: 1}
}

// scarcityWeights computes w_d = Σ fleet demand_d / Σ fleet capacity_d per
// region, once per pass — the fuller dimension weighs more, because
// misplacing the scarce resource strands hosts. Hostless demand counts too:
// it's load that must land somewhere in the region.
func (p *placer) scarcityWeights(snap stateSnapshot) regionWeights {
	if !p.cfg.ScarcityWeights {
		return nil
	}
	type sums struct{ dCPU, dMem, dDisk, cCPU, cMem, cDisk int64 }
	byRegion := map[string]*sums{}
	acc := func(region string) *sums {
		s := byRegion[region]
		if s == nil {
			s = &sums{}
			byRegion[region] = s
		}
		return s
	}
	for _, h := range snap.hosts {
		s := acc(h.Region)
		s.cCPU += int64(h.CPUMillicores)
		s.cMem += h.MemBytes
		s.cDisk += p.diskBudget(h)
	}
	for _, r := range snap.replicas {
		s := acc(r.Slot.Region)
		s.dCPU += int64(r.CPUMillicores)
		s.dMem += r.MemBytes
	}
	for _, v := range snap.volumes {
		acc(v.Region).dDisk += v.DesiredSizeBytes
	}
	rw := make(regionWeights, len(byRegion))
	for region, s := range byRegion {
		rw[region] = weights{
			cpu:  ratioOr1(s.dCPU, s.cCPU),
			mem:  ratioOr1(s.dMem, s.cMem),
			disk: ratioOr1(s.dDisk, s.cDisk),
		}
	}
	return rw
}

func ratioOr1(demand, capacity int64) float64 {
	if capacity <= 0 {
		return 1
	}
	return float64(demand) / float64(capacity)
}

// regionReference is the region's average host — the normalization base for
// the sort key, so "hard to fit" means hard relative to this fleet's hosts.
type reference struct{ cpu, mem, disk float64 }

func (p *placer) regionReference(snap stateSnapshot) map[string]reference {
	sums := map[string]*struct {
		cpu, mem, disk int64
		n              int64
	}{}
	for _, h := range snap.hosts {
		s := sums[h.Region]
		if s == nil {
			s = &struct {
				cpu, mem, disk int64
				n              int64
			}{}
			sums[h.Region] = s
		}
		s.cpu += int64(h.CPUMillicores)
		s.mem += h.MemBytes
		s.disk += p.diskBudget(h)
		s.n++
	}
	refs := make(map[string]reference, len(sums))
	for region, s := range sums {
		refs[region] = reference{
			cpu:  float64(s.cpu) / float64(s.n),
			mem:  float64(s.mem) / float64(s.n),
			disk: float64(s.disk) / float64(s.n),
		}
	}
	return refs
}

// maxNorm is the sort key: the item's tightest dimension relative to the
// region's average host. Hardest-to-fit first.
func (it packItem) maxNorm(refs map[string]reference) float64 {
	ref := refs[it.region]
	m := 0.0
	for _, d := range []struct {
		demand int64
		cap    float64
	}{{it.cpu, ref.cpu}, {it.mem, ref.mem}, {it.disk, ref.disk}} {
		if d.cap > 0 {
			m = max(m, float64(d.demand)/d.cap)
		}
	}
	return m
}

// sortItems orders the batch: replacements before new creates (they lost a
// host they already earned), hardest-to-fit next, stable ID last so placement
// stays a pure function of the snapshot.
func sortItems(items []packItem, refs map[string]reference) {
	slices.SortFunc(items, func(a, b packItem) int {
		if a.replacement != b.replacement {
			if a.replacement {
				return -1
			}
			return 1
		}
		an, bn := a.maxNorm(refs), b.maxNorm(refs)
		switch {
		case an > bn:
			return -1
		case an < bn:
			return 1
		}
		return bytes.Compare(a.id[:], b.id[:])
	})
}

// fits is constraint 1: the item fits in every dimension after subtracting
// the per-host reserve. Replacements skip the cpu/mem headroom; the disk
// reserve is never skipped — it's held for grow-only resizes, not failover.
func (p *placer) fits(it packItem, hl *hostLedger) bool {
	var rCPU, rMem int64
	if !it.replacement {
		rCPU = int64(p.cfg.Headroom * float64(hl.host.CPUMillicores))
		rMem = int64(p.cfg.Headroom * float64(hl.host.MemBytes))
	}
	if it.cpu > hl.cpu-rCPU || it.mem > hl.mem-rMem {
		return false
	}
	if it.disk > 0 {
		rDisk := int64(p.cfg.DiskReserve * float64(p.diskBudget(hl.host)))
		if it.disk > hl.disk-rDisk {
			return false
		}
	}
	return true
}

// score is the dot product Σ_d w_d · (demand_d/cap_d) · (residual_d/cap_d):
// prefer the host whose remaining shape aligns with the item's demand,
// weighted toward the region's scarce dimension.
func (p *placer) score(it packItem, hl *hostLedger, w weights) float64 {
	s := 0.0
	if c := float64(hl.host.CPUMillicores); c > 0 {
		s += w.cpu * (float64(it.cpu) / c) * (float64(hl.cpu) / c)
	}
	if c := float64(hl.host.MemBytes); c > 0 {
		s += w.mem * (float64(it.mem) / c) * (float64(hl.mem) / c)
	}
	if it.disk > 0 {
		if c := float64(p.diskBudget(hl.host)); c > 0 {
			s += w.disk * (float64(it.disk) / c) * (float64(hl.disk) / c)
		}
	}
	return s
}

// pick selects the host for one item: constraint-filter, then argmax score,
// tie broken by lowest host UUID (map iteration order must not leak into
// placement).
func (p *placer) pick(it packItem, led ledger, rw regionWeights) (uuid.UUID, bool) {
	var feasible []*hostLedger
	for _, hl := range led {
		if hl.host.Region == it.region && p.fits(it, hl) {
			feasible = append(feasible, hl)
		}
	}
	if len(feasible) == 0 {
		return uuid.Nil, false
	}

	candidates := feasible
	if it.spread {
		var clean []*hostLedger
		for _, hl := range feasible {
			if hl.slots[it.slot] == 0 {
				clean = append(clean, hl)
			}
		}
		switch {
		case len(clean) > 0:
			candidates = clean
		default:
			// Soft fallback: anti-affinity is unsatisfiable, so degrade to the
			// capacity-feasible hosts with the fewest same-slot replicas —
			// never blend affinity into the score as a weight.
			candidates = fewestSameSlot(feasible, it.slot)
		}
	}

	w := rw.get(it.region)
	best := candidates[0]
	bestScore := p.score(it, best, w)
	for _, hl := range candidates[1:] {
		s := p.score(it, hl, w)
		if s > bestScore || (s == bestScore && bytes.Compare(hl.host.ID[:], best.host.ID[:]) < 0) {
			best, bestScore = hl, s
		}
	}
	return best.host.ID, true
}

func fewestSameSlot(feasible []*hostLedger, slot replicaSlot) []*hostLedger {
	minCount := feasible[0].slots[slot]
	for _, hl := range feasible[1:] {
		minCount = min(minCount, hl.slots[slot])
	}
	var out []*hostLedger
	for _, hl := range feasible {
		if hl.slots[slot] == minCount {
			out = append(out, hl)
		}
	}
	return out
}

// placeHostless fills HostID on every assign_host intent it can satisfy and
// drops the rest — next tick retries via anyHostlessReplicas (incremental,
// Omega §5.2: all-or-nothing roughly doubles conflicts). Every other intent
// kind passes through untouched: they need no geometry.
func (p *placer) placeHostless(snap stateSnapshot, intents []Intent) []Intent {
	replicaByID := make(map[uuid.UUID]replica, len(snap.replicas))
	for _, r := range snap.replicas {
		replicaByID[r.ID] = r
	}
	volumeByID := make(map[uuid.UUID]volume, len(snap.volumes))
	for _, v := range snap.volumes {
		volumeByID[v.ID] = v
	}

	var items []packItem
	for _, it := range intents {
		if it.Kind != IntentAssignHost {
			continue
		}
		r, ok := replicaByID[it.ReplicaID]
		if !ok {
			continue
		}
		items = append(items, packItem{
			id:          r.ID,
			slot:        r.Slot,
			region:      r.Slot.Region,
			cpu:         int64(r.CPUMillicores),
			mem:         r.MemBytes,
			replacement: isReplacement(r),
			spread:      p.cfg.AntiAffinity,
		})
	}
	if len(items) == 0 {
		return intents
	}

	led := p.newLedger(snap)
	rw := p.scarcityWeights(snap)
	sortItems(items, p.regionReference(snap))

	placed := make(map[uuid.UUID]uuid.UUID, len(items))
	for _, it := range items {
		var hostID uuid.UUID
		var ok bool
		if volID := replicaByID[it.id].VolumeID; volID != uuid.Nil {
			// A replica bound to a placed volume has no choice — local volumes
			// never move, so the volume's host is the host, no filtering or
			// scoring (capacity issues surface at the DB predicate, the second
			// belt). Bound to a still-hostless volume, it stays pending on the
			// volume rather than being misplaced.
			v := volumeByID[volID]
			hostID, ok = v.HostID, led.schedulable(v.HostID)
		} else {
			hostID, ok = p.pick(it, led, rw)
		}
		if !ok {
			continue
		}
		led.commit(it, hostID)
		placed[it.id] = hostID
	}

	out := make([]Intent, 0, len(intents))
	for _, it := range intents {
		if it.Kind != IntentAssignHost {
			out = append(out, it)
			continue
		}
		hostID, ok := placed[it.ReplicaID]
		if !ok {
			continue
		}
		it.HostID = hostID
		out = append(out, it)
	}
	return out
}

// isReplacement reads a hostless replica's history off its phase: still
// pending means never placed — a new create. Any other phase means it had a
// host and lost it (host death frees replicas as non-pending) — a replacement,
// which jumps the packing queue and skips the headroom reserve.
func isReplacement(r replica) bool { return r.Phase != domain.ReplicaPhasePending }

// placeVolumes turns each hostless volume in the snapshot into a
// place_volume intent (fires once per volume — placement is permanent). Disk
// joins as a third dimension, and the service's replica cpu/mem gate the
// filter: a host with disk but no cpu is a trap — the volume lands, the
// replica never schedules. No anti-affinity: stateful services run one
// replica, nothing to spread.
func (p *placer) placeVolumes(snap stateSnapshot) []Intent {
	var items []packItem
	for _, v := range snap.volumes {
		if v.HostID != uuid.Nil {
			continue
		}
		cpu, mem := serviceDemand(snap, v)
		items = append(items, packItem{
			id:     v.ID,
			region: v.Region,
			cpu:    cpu,
			mem:    mem,
			disk:   v.DesiredSizeBytes,
		})
	}
	if len(items) == 0 {
		return nil
	}

	led := p.newLedger(snap)
	rw := p.scarcityWeights(snap)
	sortItems(items, p.regionReference(snap))

	var intents []Intent
	for _, it := range items {
		hostID, ok := p.pick(it, led, rw)
		if !ok {
			continue
		}
		// Only the disk sticks to the ledger: the replica's cpu/mem gate the
		// filter but aren't consumed here — the replica reserves them itself
		// when it places.
		led.commit(packItem{id: it.id, disk: it.disk}, hostID)
		intents = append(intents, Intent{Kind: IntentPlaceVolume, VolumeID: it.id, HostID: hostID})
	}
	return intents
}

// serviceDemand is the cpu/mem one replica of the volume's service needs in
// its region — the co-scheduling gate for volume placement.
func serviceDemand(snap stateSnapshot, v volume) (int64, int64) {
	for _, d := range snap.desired {
		if d.ServiceID == v.ServiceID && d.Slot.Region == v.Region {
			return int64(d.CPUMillicores), d.MemBytes
		}
	}
	return 0, 0
}
