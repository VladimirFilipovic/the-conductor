-- name: CreateProject :one
INSERT INTO projects (name)
VALUES ($1)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects
WHERE name = $1;

-- name: CreateEnvironment :one
INSERT INTO environments (project_name, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetEnvironment :one
SELECT * FROM environments
WHERE project_name = $1 AND name = $2;

-- name: ListEnvironments :many
SELECT * FROM environments
WHERE project_name = $1
ORDER BY name;

-- CloneEnvironmentServices copies an environment's service bindings (and their
-- per-environment source) into a freshly created one, so `environment create`
-- starts a new env with the same services rather than empty. :execrows so the
-- caller can report how many were cloned.
-- name: CloneEnvironmentServices :execrows
INSERT INTO environment_services (environment_id, service_id, source)
SELECT @dst_environment_id, es.service_id, es.source
FROM environment_services es
WHERE es.environment_id = @src_environment_id;

-- name: CreateService :one
INSERT INTO services (project_name, name, stateful)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetService :one
SELECT * FROM services
WHERE project_name = $1 AND name = $2;

-- name: ListServicesByEnvironment :many
SELECT s.* FROM services s
JOIN environment_services es ON es.service_id = s.id
JOIN environments e ON e.id = es.environment_id
WHERE e.project_name = $1 AND e.name = $2
ORDER BY s.name;

-- name: AddServiceToEnvironment :one
INSERT INTO environment_services (environment_id, service_id, source)
VALUES ($1, $2, $3)
RETURNING *;

-- ProjectStatus reports one row per (environment, service) in a project: the
-- active deploy commit (desired intent) alongside the replicas the reconcile
-- loop has actually placed (observed). The deploy/region/replica joins are
-- LEFT so a service with no current deployment still shows up (as "no deploy").
-- The @environment/@service filters are no-ops when passed empty, so the same
-- query backs the whole-project view and the narrowed one.
--
-- name: ProjectStatus :many
SELECT
	e.name                            AS environment,
	s.name                            AS service,
	s.stateful                        AS stateful,
	d.version                         AS deploy_version,
	d.status                          AS deploy_status,
	d.image_ref                       AS image_ref,
	COALESCE(dr.desired_replicas, 0)  AS desired_replicas,
	COALESCE(rc.observed_replicas, 0) AS observed_replicas,
	COALESCE(rc.healthy_replicas, 0)  AS healthy_replicas
FROM environment_services es
JOIN environments e ON e.id = es.environment_id
JOIN services     s ON s.id = es.service_id
LEFT JOIN deployments d
	ON d.environment_service_id = es.id AND d.is_current
LEFT JOIN LATERAL (
	SELECT SUM(replicas)::int AS desired_replicas
	FROM deployment_regions
	WHERE deployment_id = d.id
) dr ON true
LEFT JOIN LATERAL (
	SELECT
		COUNT(*)::int                        AS observed_replicas,
		COUNT(*) FILTER (WHERE healthy)::int AS healthy_replicas
	FROM replicas
	WHERE deployment_id = d.id AND desired_status = 'running'
) rc ON true
WHERE e.project_name = @project_name
	AND (@environment::text = '' OR e.name = @environment)
	AND (@service::text = '' OR s.name = @service)
ORDER BY e.name, s.name;
