-- Sensor-path writes: observed state flowing IN from host agents, plus the
-- staleness sweep that turns silence into scheduling signal. The reconcile
-- loop never writes observed state and the sensor never writes desired state —
-- these queries are the whole boundary.

-- Ingest a host agent's liveness ping. Status only moves between the
-- agent-owned states (ready/notready): a heartbeat must never un-cordon a host
-- an operator cordoned or is draining — those are desired-state decisions, not
-- observations.
-- name: RecordHostHeartbeat :exec
UPDATE hosts
SET last_heartbeat = @observed_at,
    status = CASE WHEN status IN ('ready', 'notready') THEN @status ELSE status END
WHERE id = @host_id;

-- First staleness threshold: a briefly-silent host stops receiving NEW work
-- (placer skips notready) but keeps its replicas — cheap and reversible, the
-- next heartbeat flips it straight back to ready. One fleet-wide statement,
-- no per-host loop. NULL last_heartbeat is excluded: a host that never
-- enrolled an agent (seeded dev fleet) isn't stale, it's static.
-- name: MarkStaleHostsNotReady :execrows
UPDATE hosts
SET status = 'notready'
WHERE status = 'ready'
  AND last_heartbeat IS NOT NULL
  AND last_heartbeat < @last_heartbeat_before;

-- Second staleness threshold: hosts silent long enough to be declared dead,
-- listed for MarkHostDown. Includes notready (the first threshold already
-- demoted them) but not cordoned/draining — those are operator desired state.
-- The EXISTS keeps the list level-triggered and self-quieting: once a host's
-- replicas are freed it stops matching, so an already-downed host isn't
-- re-marked every sweep.
-- name: ListDeadHosts :many
SELECT hosts.* FROM hosts
WHERE status IN ('ready', 'notready')
  AND last_heartbeat IS NOT NULL
  AND last_heartbeat < @last_heartbeat_before
  AND EXISTS (
      SELECT 1 FROM replicas r
      WHERE r.host_id = hosts.id
        AND r.phase NOT IN ('reaped', 'failed')
        AND r.drained_at IS NULL
  );

-- Take a dead host out of scheduling and free its replicas in one atomic
-- statement: a downed host with still-bound replicas is invisible to the
-- Reconciler (anyHostlessReplicas keys on host_id IS NULL), so the two writes
-- must never be observable apart. Replicas drop to hostless 'replacing' — the
-- placer reads it as a replacement (skips the headroom reserve), and the
-- observation guard below owns it, so a partitioned-but-alive agent can't
-- resurrect a freed replica. Already-draining replicas keep their host
-- binding: they're retiring on the drain window regardless, and unassigning
-- them would erase the drain state.
-- The WHERE re-asserts staleness: a heartbeat landing between the sweep's
-- ListStaleHosts and this write refreshes last_heartbeat, so the recovered
-- host matches nothing and keeps its replicas — downing rides on the
-- predicate, not on the sweep's possibly-stale list.
-- name: MarkHostDown :exec
WITH downed AS (
    UPDATE hosts SET status = 'notready'
    WHERE hosts.id = @host_id
      AND hosts.status IN ('ready', 'notready')
      AND hosts.last_heartbeat IS NOT NULL
      AND hosts.last_heartbeat < @last_heartbeat_before
    RETURNING hosts.id
)
UPDATE replicas
SET host_id = NULL,
    healthy = false,
    phase = 'replacing',
    revision = revision + 1
WHERE host_id IN (SELECT id FROM downed)
  AND phase NOT IN ('reaped', 'failed')
  AND drained_at IS NULL;

-- Ingest one agent-reported replica state. Terminal and orchestrator-owned
-- phases win over observations: a stale agent report must not resurrect a
-- failed replica, un-drain a retiring one, or (replacing) re-legitimize a
-- replica whose host was declared dead while its agent lives on across a
-- partition. rows-affected = 0 just means the observation was dropped as
-- stale — not an error. The healthy false→true edge stamps
-- health_checks_passed_at via the table trigger.
-- name: RecordReplicaObservation :execrows
UPDATE replicas
SET phase = @phase,
    healthy = @healthy,
    restart_count = @restart_count,
    last_exit_reason = @last_exit_reason,
    revision = revision + 1
WHERE id = @replica_id
  AND phase NOT IN ('draining', 'reaped', 'failed', 'replacing');

-- Agent discovery: every host an agent could run on. Scheduling status is
-- deliberately ignored — an agent lives on the machine regardless, and a
-- notready host's agent must be able to enroll and heartbeat or a demoted
-- host could never heal back to ready.
-- name: ListAgentHosts :many
SELECT * FROM hosts;

-- The replicas a host agent is responsible for driving (start scheduling,
-- drain draining, report the rest).
-- name: ListReplicasByHost :many
SELECT * FROM replicas
WHERE host_id = @host_id AND phase <> 'reaped';

-- Observed volume size (grow-only resize drift, §4b).
-- name: RecordVolumeObservedSize :exec
UPDATE volumes
SET observed_size_bytes = @observed_bytes
WHERE id = @volume_id;

-- Extend the lease this replica holds — the renewal a healthy observation
-- carries, so a live stateful replica's lease never runs out. Keyed by holder:
-- a lease another replica has since claimed is never touched (their upsert
-- overwrote replica_id). Extending our own lease past a silent gap is safe —
-- expiry only matters once a competitor claims the volume, and then this
-- UPDATE matches nothing. rows-affected = 0 just means "no lease held here"
-- (stateless replica, or the volume moved on) — not an error.
-- name: RenewVolumeLease :execrows
UPDATE volume_leases
SET expires_at = @expires_at
WHERE replica_id = @replica_id;
