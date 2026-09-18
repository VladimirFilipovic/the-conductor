-- Reconcile-read path. The Engine runs these three together inside a single
-- read-only REPEATABLE READ tx (storage.WithReadTx) so the desired and observed
-- halves of a pass observe one frozen snapshot — desired can't scale out from
-- under the replica list between queries, which would tear the placement diff.

-- name: SnapshotDesired :many
-- Desired state: one row per (current deployment, region) carrying the replica
-- target plus the spec the Reconciler needs to mint a replica. Joined to services
-- for the stateful flag, which selects the volume/lease placement path.
-- environment_service_id is the reconcile grouping key (a service shared across
-- environments has one current deployment per env — service_id alone would
-- collide them); it ties this desired row to its replicas in ListActiveReplicas.
SELECT
	d.environment_service_id AS environment_service_id,
	d.id                     AS deployment_id,
	svc.id                   AS service_id,
	dr.region                AS region,
	dr.replicas              AS desired_replicas,
	d.cpu_millicores         AS cpu_millicores,
	d.mem_bytes              AS mem_bytes,
	d.restart_max            AS restart_max,
	d.progress_deadline      AS progress_deadline,
	d.status                 AS status,
	svc.stateful             AS stateful
FROM deployments d
JOIN deployment_regions dr   ON dr.deployment_id = d.id
JOIN environment_services es ON es.id = d.environment_service_id
JOIN services svc            ON svc.id = es.service_id
WHERE d.is_current;

-- name: ListActiveReplicas :many
-- Observed fleet for every service that has a current deployment — INCLUDING
-- replicas still running under a superseded deployment (an in-flight rollout
-- from version N to N+1). The orchestrator groups by (environment_service_id,
-- region) and splits on is_current: is_current replicas are the new revision to
-- converge toward, the rest are the old revision to drain. Filtering on
-- d.is_current here would hide the outgoing replicas and leak them as orphans
-- nothing ever reaps. Reaped replicas are terminal and excluded.
-- host_draining rides along so the rolling cascade can treat a replica whose
-- host is being evacuated as capacity that is leaving (LEFT JOIN: a hostless
-- replica has no host to be draining).
SELECT r.*, d.environment_service_id, es.service_id, d.version, d.is_current, d.drain_seconds,
       coalesce(h.status = 'draining', false)::bool AS host_draining
FROM replicas r
JOIN deployments d           ON d.id = r.deployment_id
JOIN environment_services es ON es.id = d.environment_service_id
LEFT JOIN hosts h            ON h.id = r.host_id
WHERE r.phase <> 'reaped'
  AND EXISTS (
    SELECT 1 FROM deployments c
    WHERE c.environment_service_id = d.environment_service_id
      AND c.is_current
  );

-- name: ListActiveVolumes :many
-- Volumes for services that have a current deployment — the disks a stateful
-- placement pins and leases. Keyed by (service_id, region) against the stateful
-- rows of SnapshotDesired. DISTINCT collapses a service shared across multiple
-- environments (the lease is re-checked inside the reconcile tx regardless).
SELECT DISTINCT v.* FROM volumes v
JOIN environment_services es ON es.service_id = v.service_id
JOIN deployments d           ON d.environment_service_id = es.id
WHERE d.is_current;

-- name: ListHealthyHosts :many
-- Hosts alive this pass, operator status included: the placer's ledger holds
-- every healthy host (a volume-pinned replica must be able to return to its
-- volume's host even while that host is cordoned or draining) and filters on
-- status = 'open' only for free placement. All regions — the Engine buckets
-- by region.
SELECT * FROM hosts WHERE host_healthy;
