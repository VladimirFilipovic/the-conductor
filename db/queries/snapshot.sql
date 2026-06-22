-- Reconcile-read path. The Scheduler runs these three together inside a single
-- read-only REPEATABLE READ tx (storage.WithReadTx) so the desired and observed
-- halves of a pass observe one frozen snapshot — desired can't scale out from
-- under the replica list between queries, which would tear the placement diff.

-- name: SnapshotDesired :many
-- Desired state: one row per (current deployment, region) carrying the replica
-- target plus the spec the Scheduler needs to mint a replica. Joined to services
-- for the stateful flag, which selects the volume/lease placement path.
SELECT
	d.id             AS deployment_id,
	svc.id           AS service_id,
	dr.region        AS region,
	dr.replicas      AS desired_replicas,
	d.cpu_millicores AS cpu_millicores,
	d.mem_bytes      AS mem_bytes,
	svc.stateful     AS stateful
FROM deployments d
JOIN deployment_regions dr   ON dr.deployment_id = d.id
JOIN environment_services es ON es.id = d.environment_service_id
JOIN services svc            ON svc.id = es.service_id
WHERE d.is_current;

-- name: ListActiveReplicas :many
-- Observed fleet: every replica under a current deployment, the set the diff
-- compares against SnapshotDesired. Reaped replicas are terminal and excluded.
SELECT r.* FROM replicas r
JOIN deployments d ON d.id = r.deployment_id
WHERE d.is_current AND r.phase <> 'reaped';

-- name: ListActiveVolumes :many
-- Volumes for services that have a current deployment — the disks a stateful
-- placement pins and leases. Keyed by (service_id, region) against the stateful
-- rows of SnapshotDesired. DISTINCT collapses a service shared across multiple
-- environments (the lease is re-checked inside the reconcile tx regardless).
SELECT DISTINCT v.* FROM volumes v
JOIN environment_services es ON es.service_id = v.service_id
JOIN deployments d           ON d.environment_service_id = es.id
WHERE d.is_current;

-- name: ListSchedulableHosts :many
-- Hosts eligible to receive placements this pass: 'ready' only (notready,
-- draining, cordoned are skipped). All regions — the Scheduler buckets by region.
SELECT * FROM hosts WHERE status = 'ready';
