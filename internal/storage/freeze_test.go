package storage

// Frozen replica, against real Postgres: freeze is phase-guarded (no revision
// CAS), the frozen row is refused by the observation guard and hidden from
// the agent's HostState, and restart is the only way back — through the
// hostless replacing path, never onto a host directly.

import (
	"context"
	"errors"
	"testing"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

func (f hostFixture) freeze(t *testing.T, id uuid.UUID) error {
	t.Helper()
	ctx := context.Background()
	return f.c.WithTx(ctx, func(q Querier) error { return q.FreezeReplica(ctx, id) })
}

func (f hostFixture) fullRow(t *testing.T, id uuid.UUID) db.Replica {
	t.Helper()
	var r db.Replica
	err := f.c.pool.QueryRowContext(context.Background(),
		"SELECT host_id, phase, healthy, restart_count, revision FROM replicas WHERE id = $1", id).
		Scan(&r.HostID, &r.Phase, &r.Healthy, &r.RestartCount, &r.Revision)
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
	return r
}

// Freeze lands only on live phases: a draining or already-terminal replica
// is somebody else's (drain window, reaper) and stays put with ErrConflict.
func TestFreezeReplicaGuardsPhase(t *testing.T) {
	c := newTestClient(t)
	f := newHostFixture(t, c, "open", time.Now())
	live := f.replica(t, "starting", uuid.Nil)
	draining := f.replica(t, "draining", uuid.Nil)
	reaped := f.replica(t, "reaped", uuid.Nil)
	replacing := f.replica(t, "replacing", uuid.Nil)

	if err := f.freeze(t, live); err != nil {
		t.Fatalf("freeze(starting) = %v, want ok", err)
	}
	if r := f.fullRow(t, live); r.Phase != "failed" || r.Healthy {
		t.Fatalf("after freeze: phase=%s healthy=%v, want failed/unhealthy", r.Phase, r.Healthy)
	}
	if err := f.freeze(t, live); !errors.Is(err, ErrConflict) {
		t.Fatalf("freeze(failed) = %v, want ErrConflict (idempotent no-op)", err)
	}
	for name, id := range map[string]uuid.UUID{"draining": draining, "reaped": reaped, "replacing": replacing} {
		if err := f.freeze(t, id); !errors.Is(err, ErrConflict) {
			t.Fatalf("freeze(%s) = %v, want ErrConflict", name, err)
		}
	}
}

// The freeze closes the loop on the agent side: its reports are refused (so
// restart_count stays where it tripped) and the row leaves the host's list
// (so the agent tears the container down). ListDeploymentReplicaIDs, the
// chaos fan-out, drops it too — there is no container left to lie about.
func TestFrozenReplicaIsInvisibleToAgentPaths(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	f := newHostFixture(t, c, "open", time.Now())
	frozen := f.replica(t, "active", uuid.Nil)
	alive := f.replica(t, "active", uuid.Nil)
	if _, err := c.pool.ExecContext(ctx, "UPDATE replicas SET restart_count = 6 WHERE id = $1", frozen); err != nil {
		t.Fatalf("seed restart_count: %v", err)
	}
	if err := f.freeze(t, frozen); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	applied, err := c.RecordReplicaObservation(ctx, ReplicaObservation{
		ReplicaID: frozen, Phase: "starting", Healthy: false, RestartCount: 7,
	})
	if err != nil {
		t.Fatalf("RecordReplicaObservation: %v", err)
	}
	if applied {
		t.Fatal("observation on a frozen replica was applied; restart_count would keep climbing")
	}
	if r := f.fullRow(t, frozen); r.RestartCount != 6 || r.Phase != "failed" {
		t.Fatalf("frozen row after report: phase=%s restart_count=%d, want failed/6", r.Phase, r.RestartCount)
	}

	rows, err := c.ListReplicasByHost(ctx, f.hostID)
	if err != nil {
		t.Fatalf("ListReplicasByHost: %v", err)
	}
	for _, r := range rows {
		if r.ID == frozen {
			t.Fatal("frozen replica still listed for its host; the agent would keep restarting it")
		}
	}
	if len(rows) != 1 || rows[0].ID != alive {
		t.Fatalf("host list = %v, want only the live replica %s", rows, alive)
	}

	ids, err := c.ListDeploymentReplicaIDs(ctx, f.dep.ID)
	if err != nil {
		t.Fatalf("ListDeploymentReplicaIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != alive {
		t.Fatalf("fan-out ids = %v, want only %s", ids, alive)
	}
}

// Restart is the operator's thaw: failed → hostless replacing with a fresh
// restart budget, and nothing else — a live replica is refused (409) and an
// unknown id is not found (404), so the handler never has to guess.
func TestRestartReplicaOnlyThawsFailed(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	f := newHostFixture(t, c, "open", time.Now())
	frozen := f.replica(t, "active", uuid.Nil)
	if _, err := c.pool.ExecContext(ctx, "UPDATE replicas SET restart_count = 6, last_exit_reason = 'exit 1' WHERE id = $1", frozen); err != nil {
		t.Fatalf("seed counters: %v", err)
	}
	if err := f.freeze(t, frozen); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	before := f.fullRow(t, frozen)

	if err := c.RestartReplica(ctx, frozen); err != nil {
		t.Fatalf("RestartReplica(failed) = %v, want ok", err)
	}
	after := f.fullRow(t, frozen)
	if after.HostID.Valid || after.Phase != "replacing" || after.Healthy || after.RestartCount != 0 {
		t.Fatalf("after restart: host=%v phase=%s healthy=%v restart_count=%d, want hostless replacing with a reset budget",
			after.HostID, after.Phase, after.Healthy, after.RestartCount)
	}
	if after.Revision <= before.Revision {
		t.Fatalf("revision %d did not advance past %d; a stale drain CAS could still land", after.Revision, before.Revision)
	}

	if err := c.RestartReplica(ctx, frozen); !errors.Is(err, ErrConflict) {
		t.Fatalf("RestartReplica(replacing) = %v, want ErrConflict", err)
	}
	if err := c.RestartReplica(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RestartReplica(unknown) = %v, want ErrNotFound", err)
	}
}
