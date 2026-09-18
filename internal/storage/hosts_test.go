package storage

// Host state split, against real Postgres: host_healthy (heartbeat/watchdog)
// and status (operator) each keep their owner's last word regardless of what
// the other side writes, and the drain settles on replica rows alone.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// hostFixture is one project → deployment(us-east-1) → host chain the host
// tests hang replicas off. Cleanup order (LIFO): replicas (they pin volumes
// and hosts), volumes, project, host.
type hostFixture struct {
	c      *PostgresClient
	dep    db.Deployment
	svc    db.Service
	hostID uuid.UUID
}

func newHostFixture(t *testing.T, c *PostgresClient, status string, lastHeartbeat time.Time) hostFixture {
	t.Helper()
	ctx := context.Background()

	var hostID uuid.UUID
	err := c.pool.QueryRowContext(ctx, `
		INSERT INTO hosts (region, hostname, cpu_millicores, mem_bytes, disk_bytes, status, drain_started_at, last_heartbeat)
		VALUES ('us-east-1', $1, 4000, 8589934592, 107374182400, $2,
		        CASE WHEN $2 = 'draining' THEN $3::timestamptz ELSE NULL END, $3)
		RETURNING id`, t.Name(), status, lastHeartbeat).Scan(&hostID)
	if err != nil {
		t.Fatalf("insert host: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM hosts WHERE id = $1", hostID) })

	proj, err := c.CreateProject(ctx, t.Name())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM projects WHERE name = $1", proj.Name) })
	env, err := c.CreateEnvironment(ctx, proj.Name, "production")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	svc, err := c.CreateService(ctx, proj.Name, "web", false)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM volumes WHERE service_id = $1", svc.ID) })
	es, err := c.AddServiceToEnvironment(ctx, env.ID, svc.ID, nil)
	if err != nil {
		t.Fatalf("AddServiceToEnvironment: %v", err)
	}
	dep, err := c.CreateDeployment(ctx, db.CreateDeploymentParams{
		EnvironmentServiceID: es.ID, Version: 1, ImageRef: "nginx:latest",
		CpuMillicores: 500, MemBytes: 1 << 29,
		Env: json.RawMessage("{}"), Healthcheck: json.RawMessage("{}"),
		DrainSeconds: 30, RestartMax: 5,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if err := c.SetDeploymentRegion(ctx, dep.ID, "us-east-1", 1); err != nil {
		t.Fatalf("SetDeploymentRegion: %v", err)
	}
	t.Cleanup(func() { _, _ = c.pool.ExecContext(ctx, "DELETE FROM replicas WHERE deployment_id = $1", dep.ID) })
	return hostFixture{c: c, dep: dep, svc: svc, hostID: hostID}
}

// replica plants one replica bound to the fixture host; volumeID Nil means
// stateless.
func (f hostFixture) replica(t *testing.T, phase string, volumeID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	vol := uuid.NullUUID{UUID: volumeID, Valid: volumeID != uuid.Nil}
	err := f.c.pool.QueryRowContext(context.Background(), `
		INSERT INTO replicas (deployment_id, region, host_id, volume_id, cpu_millicores, mem_bytes, phase, healthy)
		VALUES ($1, 'us-east-1', $2, $3, 100, 1048576, $4, true)
		RETURNING id`, f.dep.ID, f.hostID, vol, phase).Scan(&id)
	if err != nil {
		t.Fatalf("insert replica: %v", err)
	}
	return id
}

func (f hostFixture) host(t *testing.T) db.Host {
	t.Helper()
	hosts, err := f.c.ListAgentHosts(context.Background())
	if err != nil {
		t.Fatalf("ListAgentHosts: %v", err)
	}
	for _, h := range hosts {
		if h.ID == f.hostID {
			return h
		}
	}
	t.Fatalf("host %s vanished", f.hostID)
	return db.Host{}
}

func (f hostFixture) replicaRow(t *testing.T, id uuid.UUID) db.Replica {
	t.Helper()
	var r db.Replica
	err := f.c.pool.QueryRowContext(context.Background(),
		"SELECT host_id, phase FROM replicas WHERE id = $1", id).Scan(&r.HostID, &r.Phase)
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
	return r
}

// A draining host that goes silent is downed like any other — dead-listed,
// replicas freed to replacing — but the death writes only host_healthy: the
// drain (status, its clock) is the operator's and survives. The heartbeat
// that brings the host back likewise touches health alone.
func TestMarkHostDownKeepsDrainingStatus(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := time.Now()
	f := newHostFixture(t, c, "draining", now.Add(-3*time.Minute))
	rep := f.replica(t, "active", uuid.Nil)

	deadCutoff := now.Add(-2 * time.Minute)
	dead, err := c.ListDeadHosts(ctx, deadCutoff)
	if err != nil {
		t.Fatalf("ListDeadHosts: %v", err)
	}
	found := false
	for _, h := range dead {
		found = found || h.ID == f.hostID
	}
	if !found {
		t.Fatal("silent draining host not dead-listed: its replicas would never be freed")
	}

	if err := c.MarkHostDown(ctx, f.hostID, deadCutoff); err != nil {
		t.Fatalf("MarkHostDown: %v", err)
	}
	if r := f.replicaRow(t, rep); r.HostID.Valid || r.Phase != "replacing" {
		t.Fatalf("replica after down: host=%v phase=%s, want hostless replacing", r.HostID, r.Phase)
	}
	h := f.host(t)
	if h.HostHealthy || h.Status != "draining" || !h.DrainStartedAt.Valid {
		t.Fatalf("host after down: healthy=%v status=%s drain_started_at=%v, want unhealthy, still draining with its clock",
			h.HostHealthy, h.Status, h.DrainStartedAt)
	}

	if err := c.RecordHostHeartbeat(ctx, f.hostID, now); err != nil {
		t.Fatalf("RecordHostHeartbeat: %v", err)
	}
	h = f.host(t)
	if !h.HostHealthy || h.Status != "draining" {
		t.Fatalf("host after heartbeat: healthy=%v status=%s, want healthy and still draining", h.HostHealthy, h.Status)
	}
}

// The drain closes on reap of the last stateless replica: a drained-but-
// running one still holds it open, a volume-pinned one never does (it isn't
// leaving), and completion clears the stalled-drain clock.
func TestCompleteDrainedHostsWaitsForReapIgnoresStateful(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := time.Now()
	f := newHostFixture(t, c, "draining", now)
	// Back-date the drain so the stalled listing has something to find.
	if _, err := c.pool.ExecContext(ctx, "UPDATE hosts SET drain_started_at = $2 WHERE id = $1", f.hostID, now.Add(-15*time.Minute)); err != nil {
		t.Fatalf("backdate drain: %v", err)
	}
	vol, err := c.CreateVolume(ctx, f.svc.ID, "data", "us-east-1", "/data", 1<<30)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	stateful := f.replica(t, "active", vol.ID)
	stateless := f.replica(t, "draining", uuid.Nil)

	completes := func(what string) []db.Host {
		t.Helper()
		done, err := c.CompleteDrainedHosts(ctx)
		if err != nil {
			t.Fatalf("CompleteDrainedHosts (%s): %v", what, err)
		}
		var mine []db.Host
		for _, h := range done {
			if h.ID == f.hostID {
				mine = append(mine, h)
			}
		}
		return mine
	}

	if got := completes("stateless still draining"); len(got) != 0 {
		t.Fatalf("drain completed while a stateless replica was still draining (not reaped): %+v", got)
	}
	stalled, err := c.ListStalledDrains(ctx, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ListStalledDrains: %v", err)
	}
	found := false
	for _, h := range stalled {
		found = found || h.ID == f.hostID
	}
	if !found {
		t.Fatal("15-minute drain not listed as stalled at a 10-minute cutoff")
	}

	if _, err := c.pool.ExecContext(ctx, "UPDATE replicas SET phase = 'reaped' WHERE id = $1", stateless); err != nil {
		t.Fatalf("reap stateless: %v", err)
	}
	got := completes("stateless reaped")
	if len(got) != 1 || got[0].Status != "cordoned" || got[0].DrainStartedAt.Valid {
		t.Fatalf("after reap: %+v, want the host returned as cordoned with drain_started_at cleared", got)
	}
	if r := f.replicaRow(t, stateful); !r.HostID.Valid || r.HostID.UUID != f.hostID {
		t.Fatalf("stateful replica moved: host=%v, want still on %s", r.HostID, f.hostID)
	}
	if got := completes("already cordoned"); len(got) != 0 {
		t.Fatalf("cordoned host re-completed: %+v", got)
	}
}

// Operator transitions: drain stamps its clock and always applies (no 409
// but from draining itself); cordon applies to open only; uncordon lifts
// both cordon and drain and clears the clock. The reservation belt lets a
// stateless replica onto open hosts only, a volume-pinned one onto any live
// host.
func TestHostTransitionsAndReservationBelt(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	f := newHostFixture(t, c, "open", time.Now())

	if ok, err := c.CordonHost(ctx, f.hostID); err != nil || !ok {
		t.Fatalf("CordonHost(open) = %v, %v; want applied", ok, err)
	}
	if ok, err := c.DrainHost(ctx, f.hostID); err != nil || !ok {
		t.Fatalf("DrainHost(cordoned) = %v, %v; want applied", ok, err)
	}
	if h := f.host(t); h.Status != "draining" || !h.DrainStartedAt.Valid {
		t.Fatalf("after drain: status=%s drain_started_at=%v", h.Status, h.DrainStartedAt)
	}
	if ok, err := c.DrainHost(ctx, f.hostID); err != nil || ok {
		t.Fatalf("DrainHost(draining) = %v, %v; want not applicable", ok, err)
	}
	if ok, err := c.CordonHost(ctx, f.hostID); err != nil || ok {
		t.Fatalf("CordonHost(draining) = %v, %v; want not applicable — would erase the drain", ok, err)
	}

	// Belt: stateless onto a draining host is refused; pinned goes through.
	vol, err := c.CreateVolume(ctx, f.svc.ID, "data", "us-east-1", "/data", 1<<30)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	pinned := f.replica(t, "replacing", vol.ID)
	free := f.replica(t, "replacing", uuid.Nil)
	for _, id := range []uuid.UUID{pinned, free} {
		if _, err := c.pool.ExecContext(ctx, "UPDATE replicas SET host_id = NULL WHERE id = $1", id); err != nil {
			t.Fatalf("unbind: %v", err)
		}
	}
	err = c.WithTx(ctx, func(q Querier) error { return q.AssignReplicaHost(ctx, free, f.hostID) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stateless reservation onto draining host = %v, want ErrConflict", err)
	}
	err = c.WithTx(ctx, func(q Querier) error { return q.AssignReplicaHost(ctx, pinned, f.hostID) })
	if err != nil {
		t.Fatalf("pinned reservation onto draining host = %v, want ok", err)
	}

	if ok, err := c.UncordonHost(ctx, f.hostID); err != nil || !ok {
		t.Fatalf("UncordonHost(draining) = %v, %v; want applied", ok, err)
	}
	if h := f.host(t); h.Status != "open" || h.DrainStartedAt.Valid {
		t.Fatalf("after uncordon: status=%s drain_started_at=%v, want open with clock cleared", h.Status, h.DrainStartedAt)
	}
	err = c.WithTx(ctx, func(q Querier) error { return q.AssignReplicaHost(ctx, free, f.hostID) })
	if err != nil {
		t.Fatalf("stateless reservation onto reopened host = %v, want ok", err)
	}
}
