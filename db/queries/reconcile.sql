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

-- Reserve a replica onto a host under optimistic concurrency: the host's
-- revision must still equal what the scheduler read while bin-packing, or the
-- host filled up under us and the whole placement must roll back. The CTE bumps
-- the host revision (the CAS) and the replica update only lands if that bump
-- matched — so rows-affected = 0 signals a lost race.
-- name: ReserveReplicaOnHost :execrows
WITH bump AS (
	UPDATE hosts SET revision = hosts.revision + 1
	WHERE hosts.id = $2 AND hosts.revision = $3
	RETURNING hosts.id
)
UPDATE replicas
SET host_id = $2, phase = 'scheduling'
WHERE replicas.id = $1 AND EXISTS (SELECT 1 FROM bump);

-- Bind a volume to the host its replica landed on. Bumps revision so a stale
-- reader detects the move.
-- name: AssignVolumeHost :exec
UPDATE volumes
SET host_id = $2, status = 'attached', revision = revision + 1
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

-- Drop a lease so a freed volume is immediately re-leasable.
-- name: ReleaseVolumeLease :exec
DELETE FROM volume_leases WHERE volume_id = $1;

-- Reclaim a replica row.
-- name: DeleteReplica :exec
DELETE FROM replicas WHERE id = $1;
