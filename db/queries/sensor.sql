-- Observed-state writes: reports flowing IN from host agents (apiserver Ingest),
-- plus the watchdog sweep that turns silence into scheduling signal. The reconcile
-- loop never writes observed state and ingest/watchdog never write desired state —
-- these queries are the whole boundary.

-- Ingest a host agent's liveness ping. Only observed state moves: status is
-- operator intent and a heartbeat must never un-cordon or un-drain a host.
-- name: RecordHostHeartbeat :exec
UPDATE hosts
SET last_heartbeat = @observed_at,
    host_healthy = true
WHERE id = @host_id;

-- First staleness threshold: a briefly-silent host stops receiving NEW work
-- (the placer's ledger holds healthy hosts only) but keeps its replicas —
-- cheap and reversible, the next heartbeat flips it straight back. One
-- fleet-wide statement, no per-host loop. NULL last_heartbeat is excluded: a
-- host that never enrolled an agent (seeded dev fleet) isn't stale, it's
-- static.
-- name: MarkStaleHostsUnhealthy :execrows
UPDATE hosts
SET host_healthy = false
WHERE host_healthy
  AND last_heartbeat IS NOT NULL
  AND last_heartbeat < @last_heartbeat_before;

-- Second staleness threshold: hosts silent long enough to be declared dead,
-- listed for MarkHostDown. Operator status is irrelevant here: a draining or
-- cordoned host that dies still holds replicas nobody else will free.
-- The EXISTS keeps the list level-triggered and self-quieting: once a host's
-- replicas are freed it stops matching, so an already-downed host isn't
-- re-marked every sweep.
-- name: ListDeadHosts :many
SELECT hosts.* FROM hosts
WHERE last_heartbeat IS NOT NULL
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
-- them would erase the drain state. status is not touched: a draining host
-- that dies is still draining when it comes back.
-- The WHERE re-asserts staleness: a heartbeat landing between the sweep's
-- ListDeadHosts and this write refreshes last_heartbeat, so the recovered
-- host matches nothing and keeps its replicas — downing rides on the
-- predicate, not on the sweep's possibly-stale list.
-- name: MarkHostDown :exec
WITH downed AS (
    UPDATE hosts SET host_healthy = false
    WHERE hosts.id = @host_id
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

-- A drain is done when nothing that can leave is left: stateless replicas
-- gone past reap (a drained one still runs until the window elapses, so it
-- counts). Stateful replicas are pinned to their volume and never move, so
-- they don't hold the drain open — the host lands in 'cordoned' with them
-- still on it, for the operator to migrate by hand. Fleet-wide, one statement.
-- name: CompleteDrainedHosts :many
UPDATE hosts SET status = 'cordoned', drain_started_at = NULL
WHERE status = 'draining'
  AND NOT EXISTS (
      SELECT 1 FROM replicas r
      WHERE r.host_id = hosts.id
        AND r.volume_id IS NULL
        AND r.phase NOT IN ('reaped', 'failed')
  )
RETURNING *;

-- Drains in flight longer than the caller's window: nothing acts on them,
-- the sweep only makes the stall visible (a region out of capacity, a
-- replacement that never turns healthy).
-- name: ListStalledDrains :many
SELECT * FROM hosts
WHERE status = 'draining' AND drain_started_at < @started_before
ORDER BY drain_started_at;

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

-- Agent discovery: every host an agent could run on. Health and status are
-- deliberately ignored — an agent lives on the machine regardless, and an
-- unhealthy host's agent must be able to enroll and heartbeat or a demoted
-- host could never heal back.
-- name: ListAgentHosts :many
SELECT * FROM hosts;

-- The replicas a host agent is responsible for driving (start scheduling,
-- drain draining, report the rest).
-- name: ListReplicasByHost :many
SELECT * FROM replicas
WHERE host_id = @host_id AND phase <> 'reaped';

-- The disks a host agent is responsible for: create on first sight, grow when
-- the control plane's target exceeds what's on disk, delete when gone. The
-- size the agent should converge to is derived in the AgentAPI from status +
-- desired/observed, so the whole row travels.
-- name: ListVolumesByHost :many
SELECT * FROM volumes
WHERE host_id = @host_id
ORDER BY id;

-- Observed volume size (grow-only resize drift, §4b): what the agent reports
-- is on disk. Unguarded — observed state has one writer per volume (its host's
-- agent) and the reconcile loop only ever reads it.
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
