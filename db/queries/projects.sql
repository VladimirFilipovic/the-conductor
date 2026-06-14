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
