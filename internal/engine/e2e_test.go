package engine

// End-to-end loop tests: the REAL pass (loadSnapshot → Reconciler → Actuator)
// and the REAL Sensor sweep run against an in-memory store — no database. The
// store mimics exactly the Postgres semantics the loop depends on: the CAS
// phase write, the guarded lease upsert, MarkHostDown's atomic unassign, the
// health high-water trigger, and tx rollback on error. The sim in
// scenarios_test.go covers the Reconciler's decisions in isolation; these
// tests prove the committed state converges.

import (
	"context"
	"database/sql"
	"maps"
	"sync"
	"testing"
	"time"

	"conductor/internal/config"
	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// --- in-memory store -------------------------------------------------------

type memDeployment struct {
	id                   uuid.UUID
	environmentServiceID uuid.UUID
	serviceID            uuid.UUID
	region               string
	replicas             int32
	cpu                  int32
	mem                  int64
	drainSeconds         int32
	stateful             bool
	status               string
	isCurrent            bool
	version              int32
}

type memReplica struct {
	id             uuid.UUID
	deploymentID   uuid.UUID
	region         string
	hostID         uuid.UUID
	volumeID       uuid.UUID
	cpu            int32
	mem            int64
	phase          domain.ReplicaPhase
	healthy        bool
	restartCount   int32
	revision       int64
	createdAt      time.Time
	drainedAt      time.Time
	healthPassedAt time.Time
}

type memVolume struct {
	id        uuid.UUID
	serviceID uuid.UUID
	region    string
	hostID    uuid.UUID
	size      int64
	status    string
}

type memLease struct {
	replicaID uuid.UUID
	expiresAt time.Time
}

// memStore is single-goroutine safe by construction in the pure e2e tests;
// the mutex exists for the gateway tests, where a real agent goroutine writes
// through the gRPC path while the test goroutine ticks and asserts.
type memStore struct {
	mu          sync.Mutex
	deployments []*memDeployment
	replicas    []*memReplica
	hosts       []db.Host
	volumes     []*memVolume
	leases      map[uuid.UUID]memLease
	served      map[replicaSlot]uuid.UUID
}

// withLock runs fn under the store lock — the concurrent tests' read/mutate
// window for direct field access.
func (m *memStore) withLock(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn()
}

func newMemStore() *memStore {
	return &memStore{
		leases: map[uuid.UUID]memLease{},
		served: map[replicaSlot]uuid.UUID{},
	}
}

func (m *memStore) deployment(id uuid.UUID) *memDeployment {
	for _, d := range m.deployments {
		if d.id == id {
			return d
		}
	}
	return nil
}

func (m *memStore) replica(id uuid.UUID) *memReplica {
	for _, r := range m.replicas {
		if r.id == id {
			return r
		}
	}
	return nil
}

func (m *memStore) volume(id uuid.UUID) *memVolume {
	for _, v := range m.volumes {
		if v.id == id {
			return v
		}
	}
	return nil
}

// --- SnapshotStore ----------------------------------------------------------

// The read side needs no isolation gymnastics: the loop is single-goroutine in
// tests, so the live state IS the frozen snapshot.
func (m *memStore) WithReadTx(_ context.Context, fn func(storage.SnapshotReader) error) error {
	return fn(m)
}

func (m *memStore) SnapshotDesired(context.Context) ([]db.SnapshotDesiredRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []db.SnapshotDesiredRow
	for _, d := range m.deployments {
		if !d.isCurrent {
			continue
		}
		rows = append(rows, db.SnapshotDesiredRow{
			EnvironmentServiceID: d.environmentServiceID,
			DeploymentID:         d.id,
			ServiceID:            d.serviceID,
			Region:               d.region,
			DesiredReplicas:      d.replicas,
			CpuMillicores:        d.cpu,
			MemBytes:             d.mem,
			RestartMax:           5,
			ProgressDeadline:     600,
			Status:               d.status,
			Stateful:             d.stateful,
		})
	}
	return rows, nil
}

func (m *memStore) ListActiveReplicas(context.Context) ([]db.ListActiveReplicasRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []db.ListActiveReplicasRow
	for _, r := range m.replicas {
		if r.phase == domain.ReplicaPhaseReaped {
			continue
		}
		d := m.deployment(r.deploymentID)
		rows = append(rows, db.ListActiveReplicasRow{
			ID:                   r.id,
			DeploymentID:         r.deploymentID,
			Region:               r.region,
			HostID:               uuid.NullUUID{UUID: r.hostID, Valid: r.hostID != uuid.Nil},
			VolumeID:             uuid.NullUUID{UUID: r.volumeID, Valid: r.volumeID != uuid.Nil},
			CpuMillicores:        r.cpu,
			MemBytes:             r.mem,
			DesiredStatus:        "running",
			Phase:                string(r.phase),
			Healthy:              r.healthy,
			RestartCount:         r.restartCount,
			Revision:             r.revision,
			CreatedAt:            r.createdAt,
			DrainedAt:            nullTime(r.drainedAt),
			HealthChecksPassedAt: nullTime(r.healthPassedAt),
			EnvironmentServiceID: d.environmentServiceID,
			ServiceID:            d.serviceID,
			Version:              d.version,
			IsCurrent:            d.isCurrent,
			DrainSeconds:         d.drainSeconds,
		})
	}
	return rows, nil
}

func (m *memStore) ListSchedulableHosts(context.Context) ([]db.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.Host
	for _, h := range m.hosts {
		if h.Status == "ready" {
			out = append(out, h)
		}
	}
	return out, nil
}

func (m *memStore) ListAgentHosts(context.Context) ([]db.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]db.Host(nil), m.hosts...), nil
}

func (m *memStore) ListActiveVolumes(context.Context) ([]db.Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.Volume
	for _, v := range m.volumes {
		out = append(out, db.Volume{
			ID:               v.id,
			ServiceID:        v.serviceID,
			Region:           v.region,
			HostID:           uuid.NullUUID{UUID: v.hostID, Valid: v.hostID != uuid.Nil},
			DesiredSizeBytes: v.size,
			Status:           v.status,
		})
	}
	return out, nil
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

// --- ActuatorStore ----------------------------------------------------------

// WithReconcileTx runs fn against a deep copy and adopts it only on success —
// the same all-or-nothing the real tx gives, which the switch-batch rollback
// test depends on.
func (m *memStore) WithReconcileTx(_ context.Context, fn func(storage.ReconcileTx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clone := m.clone()
	if err := fn(&memTx{s: clone}); err != nil {
		return err
	}
	// Adopt field by field — a struct copy would clobber the held mutex.
	m.deployments = clone.deployments
	m.replicas = clone.replicas
	m.hosts = clone.hosts
	m.volumes = clone.volumes
	m.leases = clone.leases
	m.served = clone.served
	return nil
}

func (m *memStore) clone() *memStore {
	c := newMemStore()
	for _, d := range m.deployments {
		cp := *d
		c.deployments = append(c.deployments, &cp)
	}
	for _, r := range m.replicas {
		cp := *r
		c.replicas = append(c.replicas, &cp)
	}
	for _, v := range m.volumes {
		cp := *v
		c.volumes = append(c.volumes, &cp)
	}
	c.hosts = append(c.hosts, m.hosts...)
	maps.Copy(c.leases, m.leases)
	maps.Copy(c.served, m.served)
	return c
}

type memTx struct {
	s *memStore
}

func (t *memTx) ActiveVolumeLease(_ context.Context, volumeID uuid.UUID) (db.VolumeLease, error) {
	l, ok := t.s.leases[volumeID]
	if !ok || !l.expiresAt.After(time.Now()) {
		return db.VolumeLease{}, storage.ErrNotFound
	}
	return db.VolumeLease{VolumeID: volumeID, ReplicaID: l.replicaID, ExpiresAt: l.expiresAt}, nil
}

func (t *memTx) CreateReplica(_ context.Context, spec storage.ReplicaSpec) (db.Replica, error) {
	r := &memReplica{
		id:           uuid.New(),
		deploymentID: spec.DeploymentID,
		region:       spec.Region,
		volumeID:     spec.VolumeID,
		cpu:          spec.CPUMillicores,
		mem:          spec.MemBytes,
		phase:        domain.ReplicaPhasePending,
		createdAt:    time.Now(),
	}
	t.s.replicas = append(t.s.replicas, r)
	return db.Replica{ID: r.id}, nil
}

// AssignReplicaHost mirrors the predicated reservation: host ready and
// capacity re-checked at commit time, failed replicas consuming nothing.
func (t *memTx) AssignReplicaHost(_ context.Context, replicaID, hostID uuid.UUID) error {
	r := t.s.replica(replicaID)
	if r == nil {
		return storage.ErrConflict
	}
	var h *db.Host
	for i := range t.s.hosts {
		if t.s.hosts[i].ID == hostID {
			h = &t.s.hosts[i]
		}
	}
	if h == nil || h.Status != "ready" {
		return storage.ErrConflict
	}
	var cpu, mem int64
	for _, o := range t.s.replicas {
		if o.hostID == hostID && !o.phase.Terminal() {
			cpu += int64(o.cpu)
			mem += o.mem
		}
	}
	if cpu+int64(r.cpu) > int64(h.CpuMillicores) || mem+r.mem > h.MemBytes {
		return storage.ErrConflict
	}
	r.hostID = hostID
	r.phase = domain.ReplicaPhaseScheduling
	return nil
}

func (t *memTx) AssignVolumeHost(_ context.Context, volumeID, hostID uuid.UUID) error {
	v := t.s.volume(volumeID)
	v.hostID = hostID
	v.status = "attached"
	return nil
}

func (t *memTx) AcquireVolumeLease(_ context.Context, volumeID, replicaID uuid.UUID, expiresAt time.Time) error {
	if l, ok := t.s.leases[volumeID]; ok && l.expiresAt.After(time.Now()) && l.replicaID != replicaID {
		return storage.ErrConflict
	}
	t.s.leases[volumeID] = memLease{replicaID: replicaID, expiresAt: expiresAt}
	return nil
}

func (t *memTx) SetReplicaDesiredStatus(context.Context, uuid.UUID, domain.ReplicaDesiredStatus) error {
	return nil
}

func (t *memTx) SetReplicaPhase(_ context.Context, replicaID uuid.UUID, phase domain.ReplicaPhase, expectRevision int64) error {
	r := t.s.replica(replicaID)
	if r == nil || r.revision != expectRevision {
		return storage.ErrConflict
	}
	r.phase = phase
	if phase == domain.ReplicaPhaseDraining {
		r.drainedAt = time.Now()
	}
	r.revision++
	return nil
}

func (t *memTx) ReleaseVolumeLease(_ context.Context, volumeID uuid.UUID) error {
	delete(t.s.leases, volumeID)
	return nil
}

func (t *memTx) DeleteReplica(_ context.Context, replicaID uuid.UUID) error {
	for i, r := range t.s.replicas {
		if r.id == replicaID {
			t.s.replicas = append(t.s.replicas[:i], t.s.replicas[i+1:]...)
			return nil
		}
	}
	return nil
}

func (t *memTx) SetDeploymentStatus(_ context.Context, deploymentID uuid.UUID, status domain.DeploymentStatus) error {
	t.s.deployment(deploymentID).status = string(status)
	return nil
}

func (t *memTx) SetServedRevision(_ context.Context, environmentServiceID uuid.UUID, region string, deploymentID uuid.UUID) error {
	t.s.served[replicaSlot{environmentServiceID, region}] = deploymentID
	return nil
}

var _ storage.ReconcileTx = (*memTx)(nil)

// --- SensorStore ------------------------------------------------------------

func (m *memStore) RecordHostHeartbeat(_ context.Context, hostID uuid.UUID, observedAt time.Time, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.hosts {
		h := &m.hosts[i]
		if h.ID != hostID {
			continue
		}
		h.LastHeartbeat.Time, h.LastHeartbeat.Valid = observedAt, true
		if h.Status == "ready" || h.Status == "notready" {
			h.Status = status
		}
	}
	return nil
}

func (m *memStore) MarkStaleHostsNotReady(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for i := range m.hosts {
		h := &m.hosts[i]
		if h.Status == "ready" && h.LastHeartbeat.Valid && h.LastHeartbeat.Time.Before(before) {
			h.Status = "notready"
			n++
		}
	}
	return n, nil
}

func (m *memStore) ListDeadHosts(_ context.Context, before time.Time) ([]db.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.Host
	for _, h := range m.hosts {
		if (h.Status != "ready" && h.Status != "notready") || !h.LastHeartbeat.Valid || !h.LastHeartbeat.Time.Before(before) {
			continue
		}
		// Level-triggered like the SQL EXISTS: only hosts still holding
		// freeable replicas are dead-listed, so a downed host self-quiets.
		for _, r := range m.replicas {
			if r.hostID == h.ID && !r.phase.Terminal() && r.drainedAt.IsZero() {
				out = append(out, h)
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) MarkHostDown(_ context.Context, hostID uuid.UUID, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.hosts {
		if m.hosts[i].ID != hostID {
			continue
		}
		// Staleness re-check mirrors the SQL predicate: a heartbeat that landed
		// after the sweep's list keeps the host up and its replicas bound.
		h := &m.hosts[i]
		if (h.Status != "ready" && h.Status != "notready") || !h.LastHeartbeat.Valid || !h.LastHeartbeat.Time.Before(before) {
			return nil
		}
		h.Status = "notready"
	}
	for _, r := range m.replicas {
		if r.hostID != hostID || r.phase.Terminal() || !r.drainedAt.IsZero() {
			continue
		}
		r.hostID = uuid.Nil
		r.healthy = false
		r.phase = domain.ReplicaPhaseReplacing
		r.revision++
	}
	return nil
}

func (m *memStore) RecordReplicaObservation(_ context.Context, obs storage.ReplicaObservation) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.replica(obs.ReplicaID)
	if r == nil || r.phase == domain.ReplicaPhaseDraining || r.phase == domain.ReplicaPhaseReplacing || r.phase.Terminal() {
		return false, nil // stale observation, dropped like the SQL guard
	}
	r.phase = domain.ReplicaPhase(obs.Phase)
	r.restartCount = obs.RestartCount
	r.revision++
	// The health trigger: first false→true edge stamps the high-water mark.
	if obs.Healthy && !r.healthy && r.healthPassedAt.IsZero() {
		r.healthPassedAt = time.Now()
	}
	r.healthy = obs.Healthy
	return true, nil
}

func (m *memStore) ListReplicasByHost(_ context.Context, hostID uuid.UUID) ([]db.Replica, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.Replica
	for _, r := range m.replicas {
		if r.hostID == hostID && r.phase != domain.ReplicaPhaseReaped {
			out = append(out, db.Replica{ID: r.id, Phase: string(r.phase)})
		}
	}
	return out, nil
}

func (m *memStore) RecordVolumeObservedSize(context.Context, uuid.UUID, int64) error { return nil }

func (m *memStore) RenewVolumeLease(_ context.Context, replicaID uuid.UUID, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for vol, l := range m.leases {
		if l.replicaID == replicaID {
			m.leases[vol] = memLease{replicaID: replicaID, expiresAt: expiresAt}
		}
	}
	return nil
}

var _ SensorStore = (*memStore)(nil)

// --- harness -----------------------------------------------------------------

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type loop struct {
	t      *testing.T
	ms     *memStore
	eng    *Engine
	sensor *Sensor
	clock  *fakeClock
}

func newLoop(t *testing.T) *loop {
	ms := newMemStore()
	clock := &fakeClock{t: time.Now()}
	return &loop{
		t:      t,
		ms:     ms,
		eng:    New(ms, NewReconciler(config.DefaultPlacement()), NewActuator(ms)),
		sensor: &Sensor{store: ms, now: clock.Now},
		clock:  clock,
	}
}

// tick runs one REAL engine pass: snapshot → reconcile → actuate.
func (l *loop) tick() {
	l.t.Helper()
	if err := l.eng.reconcile(context.Background()); err != nil {
		l.t.Fatalf("reconcile pass: %v", err)
	}
}

func (l *loop) addHost(region string, cpu int32, mem, disk int64) uuid.UUID {
	id := uuid.New()
	l.ms.hosts = append(l.ms.hosts, db.Host{
		ID: id, Region: region, Hostname: "host-" + id.String()[:8],
		CpuMillicores: cpu, MemBytes: mem, DiskBytes: disk, Status: "ready",
	})
	return id
}

func (l *loop) addVolume(serviceID uuid.UUID, region string, size int64) uuid.UUID {
	id := uuid.New()
	l.ms.volumes = append(l.ms.volumes, &memVolume{
		id: id, serviceID: serviceID, region: region, size: size, status: "pending",
	})
	return id
}

// deploy commits a new current deployment for the slot, superseding any prior
// one — the same shape `conductor up` writes.
func (l *loop) deploy(slot replicaSlot, serviceID uuid.UUID, n int32, stateful bool) uuid.UUID {
	version := int32(1)
	for _, d := range l.ms.deployments {
		if d.environmentServiceID != slot.EnvironmentServiceID || d.region != slot.Region {
			continue
		}
		if d.isCurrent {
			d.isCurrent = false
			d.status = string(domain.DeploymentSuperseded)
		}
		if d.version >= version {
			version = d.version + 1
		}
	}
	dep := &memDeployment{
		id:                   uuid.New(),
		environmentServiceID: slot.EnvironmentServiceID,
		serviceID:            serviceID,
		region:               slot.Region,
		replicas:             n,
		cpu:                  100,
		mem:                  1 << 20,
		drainSeconds:         0, // reap on the very next tick — no wall-clock waits
		stateful:             stateful,
		status:               string(domain.DeploymentPending),
		isCurrent:            true,
		version:              version,
	}
	l.ms.deployments = append(l.ms.deployments, dep)
	return dep.id
}

// agentConverge plays the host agents for one round: every scheduled replica
// on a live host reports in active+healthy through the real Sensor ingestion
// path.
func (l *loop) agentConverge() {
	l.t.Helper()
	ctx := context.Background()
	for _, h := range l.ms.hosts {
		if h.Status != "ready" {
			continue
		}
		reps, _ := l.ms.ListReplicasByHost(ctx, h.ID)
		for _, r := range reps {
			switch domain.ReplicaPhase(r.Phase) {
			case domain.ReplicaPhaseScheduling, domain.ReplicaPhaseStarting, domain.ReplicaPhaseHealthCheck:
				obs := storage.ReplicaObservation{ReplicaID: r.ID, Phase: string(domain.ReplicaPhaseActive), Healthy: true}
				if err := l.sensor.ObserveReplica(ctx, obs); err != nil {
					l.t.Fatalf("observe replica: %v", err)
				}
			}
		}
	}
}

// rollout drives tick+agent rounds until the deployment converges; the bound
// is generous but the loop is deterministic — extra rounds are no-ops.
func (l *loop) rollout(dep uuid.UUID) {
	l.t.Helper()
	for range 10 {
		l.tick()
		l.agentConverge()
		if l.ms.deployment(dep).status == string(domain.DeploymentActive) {
			return
		}
	}
	l.t.Fatalf("deployment %s never converged: status=%s replicas=%v",
		dep, l.ms.deployment(dep).status, l.describeReplicas())
}

func (l *loop) describeReplicas() []string {
	var out []string
	for _, r := range l.ms.replicas {
		out = append(out, string(r.phase))
	}
	return out
}

func (l *loop) replicasOf(dep uuid.UUID) []*memReplica {
	var out []*memReplica
	for _, r := range l.ms.replicas {
		if r.deploymentID == dep {
			out = append(out, r)
		}
	}
	return out
}

// --- scenarios ----------------------------------------------------------------

// A full blue/green rollout, end-to-end through committed state: v1 comes up
// canary-first and completes; v2 surges alongside, traffic switches atomically
// with the outgoing drain, v1 reaps, v2 completes.
func TestE2EBlueGreenRollout(t *testing.T) {
	l := newLoop(t)
	region := "eu-west-1"
	slot := replicaSlot{uuid.New(), region}
	serviceID := uuid.New()
	l.addHost(region, 2000, 1<<30, 1<<30)
	l.addHost(region, 2000, 1<<30, 1<<30)

	v1 := l.deploy(slot, serviceID, 2, false)
	l.rollout(v1)

	if got := l.ms.served[slot]; got != v1 {
		t.Fatalf("served revision = %s, want v1 %s", got, v1)
	}
	if n := len(l.replicasOf(v1)); n != 2 {
		t.Fatalf("v1 replicas = %d, want 2", n)
	}
	for _, r := range l.replicasOf(v1) {
		if r.hostID == uuid.Nil || !r.healthy {
			t.Fatalf("v1 replica %s not converged: host=%s healthy=%v", r.id, r.hostID, r.healthy)
		}
	}

	v2 := l.deploy(slot, serviceID, 2, false)
	l.rollout(v2)

	if got := l.ms.served[slot]; got != v2 {
		t.Fatalf("served revision = %s, want v2 %s", got, v2)
	}
	if n := len(l.replicasOf(v1)); n != 0 {
		t.Fatalf("v1 replicas leaked: %d still present", n)
	}
	if n := len(l.replicasOf(v2)); n != 2 {
		t.Fatalf("v2 replicas = %d, want 2", n)
	}
	if got := l.ms.deployment(v1).status; got != string(domain.DeploymentSuperseded) {
		t.Fatalf("v1 status = %s, want superseded", got)
	}
	if got := l.ms.deployment(v2).status; got != string(domain.DeploymentActive) {
		t.Fatalf("v2 status = %s, want active", got)
	}
}

// The sensor path: a host goes silent → the sweep marks it down and frees its
// replicas atomically → the reconciler re-places them (same rows, replacement
// priority) onto the surviving host → agents bring them back healthy.
func TestE2EHostDeathReplacement(t *testing.T) {
	l := newLoop(t)
	region := "eu-west-1"
	slot := replicaSlot{uuid.New(), region}
	serviceID := uuid.New()
	hostA := l.addHost(region, 2000, 1<<30, 1<<30)
	hostB := l.addHost(region, 2000, 1<<30, 1<<30)

	v1 := l.deploy(slot, serviceID, 2, false)
	l.rollout(v1)

	// Anti-affinity spread the pair; find who lives where.
	ctx := context.Background()
	if err := l.sensor.RecordHeartbeat(ctx, hostA, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := l.sensor.RecordHeartbeat(ctx, hostB, "ready"); err != nil {
		t.Fatal(err)
	}
	var onA []uuid.UUID
	for _, r := range l.ms.replicas {
		if r.hostID == hostA {
			onA = append(onA, r.id)
		}
	}
	if len(onA) != 1 {
		t.Fatalf("replicas on host A = %d, want 1 (anti-affinity spread)", len(onA))
	}
	lost := onA[0]

	// Host A dies: B keeps heartbeating, A goes silent past the deadline.
	l.clock.advance(hostDeadAfter + 5*time.Second)
	if err := l.sensor.RecordHeartbeat(ctx, hostB, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := l.sensor.sweepStaleHosts(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if r := l.ms.replica(lost); r.hostID != uuid.Nil || r.healthy || r.phase != domain.ReplicaPhaseReplacing {
		t.Fatalf("lost replica not freed: host=%s healthy=%v phase=%s", r.hostID, r.healthy, r.phase)
	}

	// Zombie agent: host A was only partitioned, its agent still reports the
	// replica alive. The observation guard owns 'replacing' — the report must
	// drop, not resurrect a freed replica into healthy-but-hostless.
	zombie := storage.ReplicaObservation{ReplicaID: lost, Phase: string(domain.ReplicaPhaseActive), Healthy: true}
	if err := l.sensor.ObserveReplica(ctx, zombie); err != nil {
		t.Fatalf("zombie observe: %v", err)
	}
	if r := l.ms.replica(lost); r.healthy || r.phase != domain.ReplicaPhaseReplacing {
		t.Fatalf("zombie report resurrected freed replica: healthy=%v phase=%s", r.healthy, r.phase)
	}

	// Re-place and recover — same replica row, now on the survivor.
	l.tick()
	l.agentConverge()
	l.tick()

	r := l.ms.replica(lost)
	if r == nil {
		t.Fatal("lost replica was destroyed instead of re-placed")
	}
	if r.hostID != hostB {
		t.Fatalf("lost replica on %s, want survivor %s", r.hostID, hostB)
	}
	if !r.healthy {
		t.Fatal("re-placed replica never recovered")
	}
	if got := l.ms.deployment(v1).status; got != string(domain.DeploymentActive) {
		t.Fatalf("deployment status = %s, want active (re-place is not a failed rollout)", got)
	}
}

// The stateful path: volume placement pins everything downstream. v1's volume
// lands on a host, the replica follows it with the lease; the v2 recreate
// retires v1 (lease released on reap) before the new replica takes the same
// volume, host, and lease.
func TestE2EStatefulRecreateKeepsSingleWriter(t *testing.T) {
	l := newLoop(t)
	region := "eu-west-1"
	slot := replicaSlot{uuid.New(), region}
	serviceID := uuid.New()
	l.addHost(region, 2000, 1<<30, 10<<30)
	l.addHost(region, 2000, 1<<30, 10<<30)
	vol := l.addVolume(serviceID, region, 1<<30)

	v1 := l.deploy(slot, serviceID, 1, true)
	l.rollout(v1)

	volHost := l.ms.volume(vol).hostID
	if volHost == uuid.Nil {
		t.Fatal("volume never placed")
	}
	if got := l.ms.volume(vol).status; got != "attached" {
		t.Fatalf("volume status = %s, want attached", got)
	}
	r1 := l.replicasOf(v1)[0]
	if r1.hostID != volHost {
		t.Fatalf("replica on %s, want pinned to volume host %s", r1.hostID, volHost)
	}
	if lease := l.ms.leases[vol]; lease.replicaID != r1.id {
		t.Fatalf("lease held by %s, want v1 replica %s", lease.replicaID, r1.id)
	}

	v2 := l.deploy(slot, serviceID, 1, true)
	l.rollout(v2)

	if n := len(l.replicasOf(v1)); n != 0 {
		t.Fatalf("v1 replica leaked through recreate: %d", n)
	}
	r2s := l.replicasOf(v2)
	if len(r2s) != 1 {
		t.Fatalf("v2 replicas = %d, want 1 (recreate never surges)", len(r2s))
	}
	r2 := r2s[0]
	if r2.hostID != volHost {
		t.Fatalf("v2 replica on %s, want the volume's host %s", r2.hostID, volHost)
	}
	if lease := l.ms.leases[vol]; lease.replicaID != r2.id {
		t.Fatalf("lease held by %s, want v2 replica %s (single writer handed over)", lease.replicaID, r2.id)
	}
	if got := l.ms.served[slot]; got != v2 {
		t.Fatalf("served revision = %s, want v2 %s", got, v2)
	}
}
