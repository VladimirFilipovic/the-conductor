-- Queries backing `conductor up`: it deploys a single, already-existing service
-- (created by `add`), reading its source then committing a new versioned
-- deployment. Identity comes from the folder link / flags, never the spec file.

-- GetEnvironmentService resolves the (project, environment, service) triple to
-- the binding up needs: the environment_service id to hang the deployment off,
-- the recorded source (repo/image), and whether the service is stateful.
-- name: GetEnvironmentService :one
SELECT es.id, es.source, s.id AS service_id, s.stateful
FROM environment_services es
JOIN environments e ON e.id = es.environment_id
JOIN services     s ON s.id = es.service_id
WHERE e.project_name = @project_name AND e.name = @environment AND s.name = @service;

-- NextDeploymentVersion is the per-service-env monotonic counter (§13). Cast
-- keeps sqlc from typing the COALESCE+1 as a nullable interface.
-- name: NextDeploymentVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::int AS next_version
FROM deployments
WHERE environment_service_id = $1;

-- SupersedeCurrentDeployments clears the active commit before a new one is
-- inserted as current, so the one-current-per-service partial unique index
-- never trips. Runs in the same tx as the insert.
-- name: SupersedeCurrentDeployments :exec
UPDATE deployments
SET is_current = false, status = 'superseded'
WHERE environment_service_id = $1 AND is_current;

-- name: CreateDeployment :one
INSERT INTO deployments (
	environment_service_id, version, is_current, image_ref,
	cpu_millicores, mem_bytes, env, healthcheck,
	drain_seconds, restart_max, commit_message, created_by
)
VALUES ($1, $2, true, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: SetDeploymentRegion :exec
INSERT INTO deployment_regions (deployment_id, region, replicas)
VALUES ($1, $2, $3)
ON CONFLICT (deployment_id, region) DO UPDATE SET replicas = EXCLUDED.replicas;

-- Rollback (`conductor rollback`): re-point is_current to an EXISTING older
-- deployment, no rebuild. The target row's image_ref/env/sizing are reused
-- verbatim — rollback never re-reads config.toml. The engine then converges to
-- it like any other current deployment.

-- name: GetCurrentDeployment :one
SELECT id, version FROM deployments
WHERE environment_service_id = $1 AND is_current;

-- name: GetDeploymentByVersion :one
SELECT id, version FROM deployments
WHERE environment_service_id = @environment_service_id AND version = @version;

-- PreviousDeploymentVersion is the highest version below @before — the default
-- rollback target ("the one before the current one"). No row ⇒ nothing earlier.
-- name: PreviousDeploymentVersion :one
SELECT version FROM deployments
WHERE environment_service_id = @environment_service_id AND version < @before
ORDER BY version DESC
LIMIT 1;

-- name: MarkCurrentRolledBack :exec
UPDATE deployments
SET is_current = false, status = 'rolledback'
WHERE environment_service_id = $1 AND is_current;

-- SetDeploymentCurrent promotes a past version back to current. status returns to
-- 'pending' so the engine re-runs the rollout toward it. Run after
-- MarkCurrentRolledBack in the same tx so the one-current partial index never trips.
-- name: SetDeploymentCurrent :exec
UPDATE deployments
SET is_current = true, status = 'pending'
WHERE id = $1;

-- CurrentDeploymentID resolves the (project, environment, service) triple to its
-- active deploy commit — the deployment whose deployment_regions `scale`/`down`
-- patch. No current commit (service never `up`ed) returns no rows, so callers
-- can tell the user to deploy first.
-- name: CurrentDeploymentID :one
SELECT d.id
FROM deployments d
JOIN environment_services es ON es.id = d.environment_service_id
JOIN environments        e  ON e.id = es.environment_id
JOIN services            s  ON s.id = es.service_id
WHERE e.project_name = @project_name AND e.name = @environment
  AND s.name = @service AND d.is_current;

-- ZeroDeploymentRegions drives `conductor down`: every region the current commit
-- targets goes to zero replicas, so the reconcile loop reaps the compute while
-- leaving the deployment (and any volumes) in place.
-- name: ZeroDeploymentRegions :exec
UPDATE deployment_regions SET replicas = 0
WHERE deployment_id = $1;
