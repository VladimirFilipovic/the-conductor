-- Queries backing `conductor volume`. A volume belongs to a service (not a
-- single environment — volumes.service_id) and is addressed by its mount path,
-- the contract with the container. Size is a mutable desired property the
-- reconcile loop converges to (§4b grow-only resize); there is no create-time
-- --size, so add lands a default the user patches later with `volume update`.

-- name: CreateVolume :one
INSERT INTO volumes (service_id, name, region, mount_path, desired_size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListVolumesByService :many
SELECT v.* FROM volumes v
JOIN services s ON s.id = v.service_id
WHERE s.project_name = $1 AND s.name = $2
ORDER BY v.mount_path;

-- UpdateVolumeSize patches only the desired size; the reconcile loop notices the
-- drift and flips status to 'resizing' itself. RETURNING lets the caller map a
-- missing (service, mount) to not-found instead of a silent no-op.
-- name: UpdateVolumeSize :one
UPDATE volumes
SET desired_size_bytes = @desired_size_bytes
WHERE service_id = @service_id AND mount_path = @mount_path
RETURNING *;

-- name: DeleteVolume :one
DELETE FROM volumes
WHERE service_id = @service_id AND mount_path = @mount_path
RETURNING *;
