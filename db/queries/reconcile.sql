-- Engine reconcile-path writes. These are the operations that must commit
-- together (see storage.ReconcileTx); the read/decide queries the loop runs
-- beforehand live elsewhere.

-- The live lease on a volume, if any. No row (sql.ErrNoRows) means the volume is
-- free to claim. Used inside the tx to close the TOCTOU gap before acquiring.
-- name: ActiveVolumeLease :one
SELECT * FROM volume_leases
WHERE volume_id = $1 AND expires_at > now();

-- Schedule a replica for a (deployment, region). Host/volume are bound after
-- placement is decided; phase/desired_status take their column defaults.
-- name: CreateReplica :one
INSERT INTO replicas (deployment_id, region, cpu_millicores, mem_bytes, alloc_reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- Reserve a replica onto a host. Interim shape: unguarded, pending predicated
-- reservation. The old guard was a CAS on hosts.revision, but a version proxy
-- aborts on ANY occupancy change — two same-pass placements onto one
-- half-empty host conflicted even when both fit. The plan is to carry the real
-- invariant in this UPDATE's WHERE instead:
--
--   AND EXISTS (
--     SELECT 1 FROM hosts h
--     WHERE h.id = $2 AND h.status = 'ready'
--       AND (SELECT coalesce(sum(r.cpu_millicores), 0) FROM replicas r
--            WHERE r.host_id = h.id AND r.phase NOT IN ('reaped', 'failed'))
--           + <demand cpu> <= h.cpu_millicores
--       AND (... same for mem_bytes ...)
--   )
--
-- Summing live replicas makes the check drift-proof (no reserved counter to
-- desync) and Postgres's row lock makes check-and-write atomic, so
-- rows-affected = 0 will mean "genuinely doesn't fit / host not ready" — never
-- a false abort. Until then rows-affected = 0 only means the replica row
-- vanished since the snapshot.
-- name: ReserveReplicaOnHost :execrows
UPDATE replicas
SET host_id = $2, phase = 'scheduling'
WHERE replicas.id = $1;

-- Bind a volume to the host its replica landed on.
-- name: AssignVolumeHost :exec
UPDATE volumes
SET host_id = $2, status = 'attached'
WHERE id = $1;

-- Take (or renew) the single-writer lease. The PK on volume_id is the invariant;
-- the upsert only overwrites a lease that has expired or that this same replica
-- already holds, so a different live holder yields rows-affected = 0.
-- name: AcquireVolumeLease :execrows
INSERT INTO volume_leases (volume_id, replica_id, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT (volume_id) DO UPDATE
	SET replica_id  = EXCLUDED.replica_id,
	    expires_at  = EXCLUDED.expires_at,
	    acquired_at = now()
WHERE volume_leases.expires_at <= now()
   OR volume_leases.replica_id = EXCLUDED.replica_id;

-- Flip a replica's desired status (e.g. to 'running' once its lease is held, or
-- 'stopped' to begin reclaim).
-- name: SetReplicaDesiredStatus :exec
UPDATE replicas SET desired_status = $2 WHERE id = $1;

-- Advance a replica's lifecycle phase under optimistic concurrency: the write
-- only lands if revision still matches what the loop read, so a phase decided
-- against stale observed state (the Sensor moved it meanwhile) is dropped rather
-- than clobbering. rows-affected = 0 signals the lost race.
-- name: SetReplicaPhase :execrows
-- Stamps drained_at when (and only when) entering draining, so the drain-window
-- rule can reap at drained_at + drain_seconds; other transitions leave it intact.
UPDATE replicas
SET phase = $2,
    drained_at = CASE WHEN $2 = 'draining' THEN now() ELSE drained_at END,
    revision = revision + 1
WHERE id = $1 AND revision = $3;

-- Flip the slot's traffic switch to a new deployment. Runs in the same tx as
-- the outgoing drain batch so the shift is atomic with retiring the old side.
-- name: SetServedRevision :exec
INSERT INTO served_revisions (environment_service_id, region, deployment_id)
VALUES ($1, $2, $3)
ON CONFLICT (environment_service_id, region) DO UPDATE
	SET deployment_id = EXCLUDED.deployment_id,
	    updated_at    = now();

-- The deployment a slot's traffic currently points at (router / status reads).
-- name: GetServedRevision :one
SELECT * FROM served_revisions
WHERE environment_service_id = $1 AND region = $2;

-- Drop a lease so a freed volume is immediately re-leasable.
-- name: ReleaseVolumeLease :exec
DELETE FROM volume_leases WHERE volume_id = $1;

-- Reclaim a replica row.
-- name: DeleteReplica :exec
DELETE FROM replicas WHERE id = $1;
