-- name: CreateProject :one
INSERT INTO projects (name)
VALUES ($1)
RETURNING *;

-- name: CreateEnvironment :one
INSERT INTO environments (project_name, name)
VALUES ($1, $2)
RETURNING *;

-- name: CreateService :one
INSERT INTO services (project_name, name, stateful)
VALUES ($1, $2, $3)
RETURNING *;

-- name: AddServiceToEnvironment :one
INSERT INTO environment_services (environment_id, service_id, source)
VALUES ($1, $2, $3)
RETURNING *;
