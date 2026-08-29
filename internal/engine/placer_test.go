package engine

// Placer tests: pure table tests on (snapshot, batch) → intents, no DB.
// UUIDs are pinned (first byte = n) because placement tie-breaks on lowest
// UUID — determinism is part of the contract under test.

import (
	"testing"

	"conductor/internal/config"
	"conductor/internal/domain"

	"github.com/google/uuid"
)

func pinnedID(n byte) uuid.UUID { return uuid.UUID{n} }

// flatPlacement strips every knob that isn't under test; individual tests
// switch on the one they exercise.
func flatPlacement() config.Placement {
	return config.Placement{
		Headroom:        0,
		AntiAffinity:    true,
		ScarcityWeights: false,
		VolumeBudget:    0.8,
		DiskReserve:     0.2,
	}
}

func testHost(n byte, region string, cpu int32, mem, disk int64) host {
	return host{ID: pinnedID(n), Region: region, CPUMillicores: cpu, MemBytes: mem, DiskBytes: disk}
}

func hostlessReplica(n byte, slot replicaSlot, cpu int32, mem int64, phase domain.ReplicaPhase) replica {
	return replica{ID: pinnedID(n), Slot: slot, CPUMillicores: cpu, MemBytes: mem, Phase: phase}
}

func assignIntents(rs ...replica) []Intent {
	intents := make([]Intent, len(rs))
	for i, r := range rs {
		intents[i] = Intent{Kind: IntentAssignHost, ReplicaID: r.ID}
	}
	return intents
}

// placedHosts maps replica → chosen host from a placeHostless result.
func placedHosts(t *testing.T, intents []Intent) map[uuid.UUID]uuid.UUID {
	t.Helper()
	got := make(map[uuid.UUID]uuid.UUID, len(intents))
	for _, it := range intents {
		if it.Kind != IntentAssignHost {
			continue
		}
		if it.HostID == uuid.Nil {
			t.Fatalf("assign_host intent for %s left without HostID", it.ReplicaID)
		}
		got[it.ReplicaID] = it.HostID
	}
	return got
}

// A batch competing for one host must see each other's placements: without
// the shared ledger both 600m replicas would land on the same 1000m host.
func TestPlaceBatchSharesLedger(t *testing.T) {
	region := "eu"
	slotA := replicaSlot{uuid.New(), region}
	slotB := replicaSlot{uuid.New(), region}
	r1 := hostlessReplica(10, slotA, 600, 100, domain.ReplicaPhasePending)
	r2 := hostlessReplica(11, slotB, 600, 100, domain.ReplicaPhasePending)
	snap := stateSnapshot{
		hosts: []host{
			testHost(1, region, 1000, 1<<30, 0),
			testHost(2, region, 1000, 1<<30, 0),
		},
		replicas: []replica{r1, r2},
	}

	p := placer{cfg: flatPlacement()}
	got := placedHosts(t, p.placeHostless(snap, assignIntents(r1, r2)))

	if len(got) != 2 {
		t.Fatalf("placed %d replicas, want 2", len(got))
	}
	if got[r1.ID] == got[r2.ID] {
		t.Fatalf("both replicas landed on %s — ledger not shared within the pass", got[r1.ID])
	}
}

// The stuck-sort case: a big item whose only viable host would be taken by a
// small one placed first. Best-fit *decreasing* places the hard item first,
// so both fit.
func TestPlaceHardestToFitFirst(t *testing.T) {
	region := "eu"
	big := hostlessReplica(1, replicaSlot{uuid.New(), region}, 900, 0, domain.ReplicaPhasePending)
	small := hostlessReplica(2, replicaSlot{uuid.New(), region}, 400, 0, domain.ReplicaPhasePending)
	bigOnly := testHost(1, region, 1000, 1<<30, 0)
	smallOnly := testHost(2, region, 500, 1<<30, 0)
	snap := stateSnapshot{
		hosts:    []host{bigOnly, smallOnly},
		replicas: []replica{big, small},
	}

	p := placer{cfg: flatPlacement()}
	// small first in the batch: sorting, not batch order, must decide.
	got := placedHosts(t, p.placeHostless(snap, assignIntents(small, big)))

	if got[big.ID] != bigOnly.ID {
		t.Fatalf("big replica on %s, want %s (starved by the small one)", got[big.ID], bigOnly.ID)
	}
	if got[small.ID] != smallOnly.ID {
		t.Fatalf("small replica on %s, want %s", got[small.ID], smallOnly.ID)
	}
}

// Three same-slot replicas in one tick must spread over three hosts even
// though any single host could hold them all — the ledger's same-slot index
// is what makes anti-affinity see this tick's own placements.
func TestPlaceLedgerAntiAffinity(t *testing.T) {
	region := "eu"
	slot := replicaSlot{uuid.New(), region}
	r1 := hostlessReplica(10, slot, 100, 100, domain.ReplicaPhasePending)
	r2 := hostlessReplica(11, slot, 100, 100, domain.ReplicaPhasePending)
	r3 := hostlessReplica(12, slot, 100, 100, domain.ReplicaPhasePending)
	snap := stateSnapshot{
		hosts: []host{
			testHost(1, region, 10_000, 1<<30, 0),
			testHost(2, region, 10_000, 1<<30, 0),
			testHost(3, region, 10_000, 1<<30, 0),
		},
		replicas: []replica{r1, r2, r3},
	}

	p := placer{cfg: flatPlacement()}
	got := placedHosts(t, p.placeHostless(snap, assignIntents(r1, r2, r3)))

	seen := map[uuid.UUID]bool{}
	for _, h := range got {
		if seen[h] {
			t.Fatalf("two same-slot replicas on host %s", h)
		}
		seen[h] = true
	}
	if len(seen) != 3 {
		t.Fatalf("used %d hosts, want 3", len(seen))
	}
}

// When anti-affinity is unsatisfiable the soft fallback spreads evenly:
// 20 same-slot replicas over 5 hosts → 4 each, never a pile-up.
func TestPlaceFallbackDistributesEvenly(t *testing.T) {
	region := "eu"
	slot := replicaSlot{uuid.New(), region}
	var replicas []replica
	for i := range 20 {
		replicas = append(replicas, hostlessReplica(byte(100+i), slot, 100, 100, domain.ReplicaPhasePending))
	}
	var hosts []host
	for i := range 5 {
		hosts = append(hosts, testHost(byte(1+i), region, 100_000, 1<<30, 0))
	}
	snap := stateSnapshot{hosts: hosts, replicas: replicas}

	p := placer{cfg: flatPlacement()}
	got := placedHosts(t, p.placeHostless(snap, assignIntents(replicas...)))

	perHost := map[uuid.UUID]int{}
	for _, h := range got {
		perHost[h]++
	}
	if len(got) != 20 {
		t.Fatalf("placed %d, want 20", len(got))
	}
	for h, n := range perHost {
		if n != 4 {
			t.Fatalf("host %s got %d replicas, want 4 (distribution %v)", h, n, perHost)
		}
	}
}

// The headroom reserve blocks a normal create but never a replacement — the
// reserve exists exactly for replicas re-placed after a host death.
func TestReserveBlocksCreateNotReplacement(t *testing.T) {
	region := "eu"
	slot := replicaSlot{uuid.New(), region}
	cfg := flatPlacement()
	cfg.Headroom = 0.1 // usable: 900 of 1000 millicores

	tests := []struct {
		name   string
		phase  domain.ReplicaPhase
		placed bool
	}{
		{"pending create is blocked by the reserve", domain.ReplicaPhasePending, false},
		{"replacement skips the reserve", domain.ReplicaPhaseStarting, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := hostlessReplica(10, slot, 950, 100, tt.phase)
			snap := stateSnapshot{
				hosts:    []host{testHost(1, region, 1000, 1<<30, 0)},
				replicas: []replica{r},
			}
			p := placer{cfg: cfg}
			got := placedHosts(t, p.placeHostless(snap, assignIntents(r)))
			if _, ok := got[r.ID]; ok != tt.placed {
				t.Fatalf("placed = %v, want %v", ok, tt.placed)
			}
		})
	}
}

// Scarcity weights must be able to flip a placement: when cpu is the scarce
// dimension fleet-wide, the cpu-aligned host wins even though the raw dot
// product prefers the mem-aligned one.
func TestScarcityFlipsPlacement(t *testing.T) {
	region := "eu"
	cpuRich := testHost(1, region, 1000, 1000, 0)
	memRich := testHost(2, region, 1000, 1000, 0)
	// mem is plentiful fleet-wide (the third host), cpu is not; its zero cpu
	// keeps it out of the candidate set.
	memPool := testHost(3, region, 0, 100_000, 0)

	item := hostlessReplica(10, replicaSlot{uuid.New(), region}, 100, 200, domain.ReplicaPhasePending)
	snap := stateSnapshot{
		hosts: []host{cpuRich, memRich, memPool},
		replicas: []replica{
			{ID: pinnedID(20), Slot: replicaSlot{uuid.New(), region}, HostID: cpuRich.ID, CPUMillicores: 100, MemBytes: 700},
			{ID: pinnedID(21), Slot: replicaSlot{uuid.New(), region}, HostID: memRich.ID, CPUMillicores: 800, MemBytes: 100},
			item,
		},
	}

	unweighted := flatPlacement()
	weighted := flatPlacement()
	weighted.ScarcityWeights = true

	pu := placer{cfg: unweighted}
	if got := placedHosts(t, pu.placeHostless(snap, assignIntents(item))); got[item.ID] != memRich.ID {
		t.Fatalf("unweighted placement on %s, want mem-aligned %s", got[item.ID], memRich.ID)
	}
	pw := placer{cfg: weighted}
	if got := placedHosts(t, pw.placeHostless(snap, assignIntents(item))); got[item.ID] != cpuRich.ID {
		t.Fatalf("weighted placement on %s, want cpu-aligned %s", got[item.ID], cpuRich.ID)
	}
}

// An unplaceable item is dropped — the rest of the batch and every
// non-assign intent still flow through untouched.
func TestPlaceDropsUnplaceableKeepsRest(t *testing.T) {
	region := "eu"
	tooBig := hostlessReplica(10, replicaSlot{uuid.New(), region}, 2000, 100, domain.ReplicaPhasePending)
	fitting := hostlessReplica(11, replicaSlot{uuid.New(), region}, 100, 100, domain.ReplicaPhasePending)
	drain := Intent{Kind: IntentDrain, ReplicaID: pinnedID(30)}
	snap := stateSnapshot{
		hosts:    []host{testHost(1, region, 1000, 1<<30, 0)},
		replicas: []replica{tooBig, fitting},
	}

	p := placer{cfg: flatPlacement()}
	out := p.placeHostless(snap, append(assignIntents(tooBig, fitting), drain))

	if len(out) != 2 {
		t.Fatalf("intents out = %d, want 2 (fitting assign + drain)", len(out))
	}
	got := placedHosts(t, out)
	if _, ok := got[tooBig.ID]; ok {
		t.Fatal("unplaceable replica was placed")
	}
	if _, ok := got[fitting.ID]; !ok {
		t.Fatal("fitting replica was dropped with the unplaceable one")
	}
	if out[len(out)-1] != drain {
		t.Fatalf("drain intent not passed through untouched: %v", out[len(out)-1])
	}
}

// A stateful replica bound to a placed volume is pinned to the volume's host;
// bound to a still-hostless volume it stays pending on the volume instead of
// being misplaced.
func TestPinnedReplicaFollowsVolume(t *testing.T) {
	region := "eu"
	slot := replicaSlot{uuid.New(), region}
	volHost := testHost(2, region, 1000, 1<<30, 1<<30)
	otherHost := testHost(1, region, 10_000, 1<<30, 1<<30)

	tests := []struct {
		name       string
		volumeHost uuid.UUID
		wantHost   uuid.UUID
		wantPlaced bool
	}{
		{"placed volume pins the replica", volHost.ID, volHost.ID, true},
		{"hostless volume holds the replica back", uuid.Nil, uuid.Nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vol := volume{ID: pinnedID(40), ServiceID: uuid.New(), Region: region, HostID: tt.volumeHost, DesiredSizeBytes: 1 << 20}
			r := hostlessReplica(10, slot, 100, 100, domain.ReplicaPhasePending)
			r.VolumeID = vol.ID
			snap := stateSnapshot{
				hosts:    []host{otherHost, volHost},
				replicas: []replica{r},
				volumes:  []volume{vol},
			}
			p := placer{cfg: flatPlacement()}
			got := placedHosts(t, p.placeHostless(snap, assignIntents(r)))
			h, ok := got[r.ID]
			if ok != tt.wantPlaced {
				t.Fatalf("placed = %v, want %v", ok, tt.wantPlaced)
			}
			if ok && h != tt.wantHost {
				t.Fatalf("pinned to %s, want volume host %s", h, tt.wantHost)
			}
		})
	}
}

// --- placeVolumes ---

func placedVolumes(t *testing.T, intents []Intent) map[uuid.UUID]uuid.UUID {
	t.Helper()
	got := make(map[uuid.UUID]uuid.UUID, len(intents))
	for _, it := range intents {
		if it.Kind != IntentPlaceVolume {
			t.Fatalf("placeVolumes emitted %s, want only place_volume", it.Kind)
		}
		got[it.VolumeID] = it.HostID
	}
	return got
}

// A disk-rich host whose cpu can't fit the service's replica is a trap: the
// volume would land, the replica never schedule. The balanced host wins.
func TestVolumeFilterRejectsCPUStarvedHost(t *testing.T) {
	region := "eu"
	serviceID := uuid.New()
	diskRich := testHost(1, region, 1000, 1<<30, 100<<30)
	balanced := testHost(2, region, 1000, 1<<30, 10<<30)
	snap := stateSnapshot{
		hosts: []host{diskRich, balanced},
		desired: []desiredState{{
			Slot:          replicaSlot{uuid.New(), region},
			ServiceID:     serviceID,
			CPUMillicores: 100,
			MemBytes:      100,
		}},
		replicas: []replica{
			// Eats diskRich's cpu so the replica gate must reject it.
			{ID: pinnedID(20), Slot: replicaSlot{uuid.New(), region}, HostID: diskRich.ID, CPUMillicores: 950},
		},
		volumes: []volume{{ID: pinnedID(40), ServiceID: serviceID, Region: region, DesiredSizeBytes: 1 << 30}},
	}

	p := placer{cfg: flatPlacement()}
	got := placedVolumes(t, p.placeVolumes(snap))
	if got[pinnedID(40)] != balanced.ID {
		t.Fatalf("volume on %s, want balanced %s", got[pinnedID(40)], balanced.ID)
	}
}

// The volume budget boundary is exact: a new volume fits at
// disk·budget − reserve and is rejected one byte over.
func TestVolumeBudgetBoundary(t *testing.T) {
	region := "eu"
	// disk 1000 → budget 800, reserve 160 → 640 usable by new volumes.
	h := testHost(1, region, 1000, 1<<30, 1000)

	tests := []struct {
		name   string
		size   int64
		placed bool
	}{
		{"exactly at the boundary", 640, true},
		{"one byte over", 641, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := stateSnapshot{
				hosts:   []host{h},
				volumes: []volume{{ID: pinnedID(40), ServiceID: uuid.New(), Region: region, DesiredSizeBytes: tt.size}},
			}
			p := placer{cfg: flatPlacement()}
			got := placedVolumes(t, p.placeVolumes(snap))
			if _, ok := got[pinnedID(40)]; ok != tt.placed {
				t.Fatalf("placed = %v, want %v", ok, tt.placed)
			}
		})
	}
}

// A grow-only resize may dip into the disk reserve, so a placed volume can
// legitimately sit past the new-volume line — and a new volume must then be
// rejected even though raw budget space remains.
func TestResizedVolumeBlocksNewVolume(t *testing.T) {
	region := "eu"
	// budget 800, reserve 160; the resized volume already holds 700.
	h := testHost(1, region, 1000, 1<<30, 1000)
	snap := stateSnapshot{
		hosts: []host{h},
		volumes: []volume{
			{ID: pinnedID(40), ServiceID: uuid.New(), Region: region, HostID: h.ID, DesiredSizeBytes: 700},
			{ID: pinnedID(41), ServiceID: uuid.New(), Region: region, DesiredSizeBytes: 50},
		},
	}

	p := placer{cfg: flatPlacement()}
	if got := placedVolumes(t, p.placeVolumes(snap)); len(got) != 0 {
		t.Fatalf("new volume placed into the resize reserve: %v", got)
	}
}

// Two volumes in one tick share the ledger: the second sees the disk the
// first consumed and lands on the other host.
func TestVolumesShareLedger(t *testing.T) {
	region := "eu"
	cfg := flatPlacement()
	cfg.DiskReserve = 0 // budget 800 per host, fully usable
	hA := testHost(1, region, 1000, 1<<30, 1000)
	hB := testHost(2, region, 1000, 1<<30, 1000)
	snap := stateSnapshot{
		hosts: []host{hA, hB},
		volumes: []volume{
			{ID: pinnedID(40), ServiceID: uuid.New(), Region: region, DesiredSizeBytes: 500},
			{ID: pinnedID(41), ServiceID: uuid.New(), Region: region, DesiredSizeBytes: 500},
		},
	}

	p := placer{cfg: cfg}
	got := placedVolumes(t, p.placeVolumes(snap))
	if len(got) != 2 {
		t.Fatalf("placed %d volumes, want 2", len(got))
	}
	if got[pinnedID(40)] == got[pinnedID(41)] {
		t.Fatalf("both volumes on %s — ledger not shared", got[pinnedID(40)])
	}
}

// Placement is permanent: a volume with a host produces no intent, and a
// volume nothing can hold is dropped for the next tick.
func TestVolumeSteadyStates(t *testing.T) {
	region := "eu"
	h := testHost(1, region, 1000, 1<<30, 1000)

	tests := []struct {
		name string
		vol  volume
	}{
		{"placed volume is never re-placed", volume{ID: pinnedID(40), Region: region, HostID: h.ID, DesiredSizeBytes: 100}},
		{"unplaceable volume stays hostless", volume{ID: pinnedID(41), Region: region, DesiredSizeBytes: 10_000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := stateSnapshot{hosts: []host{h}, volumes: []volume{tt.vol}}
			p := placer{cfg: flatPlacement()}
			if got := p.placeVolumes(snap); len(got) != 0 {
				t.Fatalf("intents = %v, want none", got)
			}
		})
	}
}
