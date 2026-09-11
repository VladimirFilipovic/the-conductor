-- Control-plane API reads and operator writes (internal/api/controlplane.go),
-- plus the apiserver liveness the sensor's startup grace keys on.

-- Fleet-wide replica listing for the read API; reaped rows are history.
-- name: ListReplicas :many
SELECT * FROM replicas
WHERE phase <> 'reaped'
ORDER BY created_at;

-- Operator takes a host out of scheduling while its replicas keep running.
-- Only agent-owned states may be cordoned: a draining host is already leaving
-- and re-labelling it would erase that intent. rows = 0 means "not applicable".
-- name: CordonHost :execrows
UPDATE hosts SET status = 'cordoned'
WHERE id = @host_id AND status IN ('ready', 'notready');

-- Operator hands a host back to scheduling; it returns as 'ready' and the next
-- heartbeat (or its absence) corrects that within one sweep. Draining is
-- included so an evacuation can be called off — without this a drained host
-- had no way back except SQL.
-- name: UncordonHost :execrows
UPDATE hosts SET status = 'ready'
WHERE id = @host_id AND status IN ('cordoned', 'draining');

-- Operator evacuates a host: the reconciler drains its replicas elsewhere.
-- name: DrainHost :execrows
UPDATE hosts SET status = 'draining'
WHERE id = @host_id AND status <> 'draining';

-- name: RegisterGatewayInstance :exec
INSERT INTO gateway_instances (id, started_at, heartbeat_at)
VALUES (@id, @now, @now);

-- name: HeartbeatGatewayInstance :exec
UPDATE gateway_instances SET heartbeat_at = @now WHERE id = @id;

-- name: DeregisterGatewayInstance :exec
DELETE FROM gateway_instances WHERE id = @id;

-- Instances silent past the liveness window are gone (crashed without
-- deregistering); reap them so they can't anchor the grace computation.
-- name: DeleteStaleGatewayInstances :exec
DELETE FROM gateway_instances WHERE heartbeat_at < @heartbeat_before;

-- The oldest start among instances heartbeating recently. No row = no live
-- gateway, so no host could have heartbeated and no death verdict is fair.
-- name: OldestLiveGatewayStart :one
SELECT started_at FROM gateway_instances
WHERE heartbeat_at >= @heartbeat_after
ORDER BY started_at
LIMIT 1;

-- Topology / meta reads backing the operator UI. Every filter is a no-op when
-- passed empty, so one query serves both the "everything" view and a narrowed
-- one (same convention as ProjectStatus).

-- name: ListProjectNames :many
SELECT name FROM projects
WHERE (@project::text = '' OR name = @project)
ORDER BY name;

-- name: ListRegionNames :many
SELECT name FROM regions
ORDER BY name;

-- name: ListEnvironmentRows :many
SELECT id, project_name, name FROM environments
WHERE (@project::text = '' OR project_name = @project)
  AND (@environment::text = '' OR name = @environment)
ORDER BY project_name, name;

-- One row per service binding plus its active deploy commit; the deployment
-- join is LEFT so a bound-but-never-deployed service still appears.
-- name: TopologyServices :many
SELECT es.id AS es_id, es.environment_id, s.name AS service, s.stateful,
       d.id AS deployment_id, d.version, d.status, d.image_ref, d.created_at,
       d.created_by, d.commit_message
FROM environment_services es
JOIN environments e ON e.id = es.environment_id
JOIN services     s ON s.id = es.service_id
LEFT JOIN deployments d ON d.environment_service_id = es.id AND d.is_current
WHERE (@project::text = '' OR e.project_name = @project)
  AND (@environment::text = '' OR e.name = @environment)
ORDER BY s.name;

-- Desired replica counts of the active commits, per region.
-- name: TopologyDesiredRegions :many
SELECT es.id AS es_id, dr.region, dr.replicas AS desired
FROM deployment_regions dr
JOIN deployments          d  ON d.id = dr.deployment_id AND d.is_current
JOIN environment_services es ON es.id = d.environment_service_id
JOIN environments         e  ON e.id = es.environment_id
WHERE (@project::text = '' OR e.project_name = @project)
  AND (@environment::text = '' OR e.name = @environment)
  AND (@region::text = '' OR dr.region = @region);

-- Every replica of every version (not just the current one): a superseded or
-- draining replica is exactly what the operator wants to watch disappear.
-- name: TopologyReplicas :many
SELECT r.id, r.region, h.hostname, r.host_id, r.phase, r.healthy,
       r.desired_status, r.restart_count, r.last_exit_reason, r.updated_at,
       d.version AS dep_version, d.is_current, d.id AS deployment_id,
       es.id AS es_id
FROM replicas r
JOIN deployments          d  ON d.id = r.deployment_id
JOIN environment_services es ON es.id = d.environment_service_id
JOIN environments         e  ON e.id = es.environment_id
LEFT JOIN hosts h ON h.id = r.host_id
WHERE (@project::text = '' OR e.project_name = @project)
  AND (@environment::text = '' OR e.name = @environment)
  AND (@region::text = '' OR r.region = @region)
ORDER BY r.region, r.created_at;

-- name: TopologyHosts :many
SELECT * FROM hosts
WHERE (@region::text = '' OR region = @region)
ORDER BY region, hostname;

-- Traffic pointers, so the UI can flag a slot still serving an older version.
-- name: TopologyServed :many
SELECT sr.environment_service_id, sr.region, sr.deployment_id,
       d.version AS dep_version, s.name AS service, e.name AS environment,
       sr.updated_at
FROM served_revisions sr
JOIN deployments          d  ON d.id = sr.deployment_id
JOIN environment_services es ON es.id = sr.environment_service_id
JOIN environments         e  ON e.id = es.environment_id
JOIN services             s  ON s.id = es.service_id
WHERE (@project::text = '' OR e.project_name = @project)
  AND (@environment::text = '' OR e.name = @environment)
ORDER BY e.name, s.name, sr.region;

-- Project-wide service listing for the bind form: it must offer services NOT
-- yet in the environment, so the environment filter is opt-in.
-- name: ListProjectServices :many
SELECT s.id, s.name, s.stateful
FROM services s
WHERE s.project_name = @project
  AND (@environment::text = '' OR EXISTS (
        SELECT 1
        FROM environment_services es
        JOIN environments e ON e.id = es.environment_id
        WHERE es.service_id = s.id
          AND e.project_name = s.project_name
          AND e.name = @environment))
ORDER BY s.name;

-- Fan-out targets for deployment-wide agent chaos. Terminal phases are excluded:
-- a reaped or failed replica has no agent left to lie about it.
-- name: ListDeploymentReplicaIDs :many
SELECT id FROM replicas
WHERE deployment_id = @deployment_id AND phase NOT IN ('reaped', 'failed');
