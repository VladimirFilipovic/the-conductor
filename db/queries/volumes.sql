-- Queries backing `conductor volume`. A volume belongs to a service (not a
-- single environment — volumes.service_id) and is addressed by its mount path,
-- the contract with the container. Size is a mutable desired property the
-- reconcile loop converges to (§4b grow-only resize): `volume update` writes
-- desired, `volume revert` takes an unapproved grow back, and the engine alone
-- moves status.

-- name: CreateVolume :one
INSERT INTO volumes (service_id, name, region, mount_path, desired_size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListVolumesByService :many
SELECT v.* FROM volumes v
JOIN services s ON s.id = v.service_id
WHERE s.project_name = $1 AND s.name = $2
ORDER BY v.mount_path;

-- UpdateVolumeSize patches only the desired size and remembers the size it
-- replaced (the one-shot revert target). Status is untouched: the engine is
-- its only writer. It notices the drift (desired > observed) on its next
-- tick and either approves the grow (→ 'resizing') or parks it
-- (→ 'resize_pending') until the host has room; the agent's report of the
-- new size settles it back to 'attached'. RETURNING lets the caller map a
-- missing (service, mount) to not-found instead of a silent no-op.
-- name: UpdateVolumeSize :one
UPDATE volumes
SET previous_desired_size_bytes = desired_size_bytes,
    desired_size_bytes          = @desired_size_bytes
WHERE service_id = @service_id AND mount_path = @mount_path
RETURNING *;

-- RevertVolumeSize takes back a grow the host could not hold: desired returns
-- to what it was before `volume update`, and the revert target is consumed so
-- it works exactly once. Only a 'resize_pending' volume qualifies — resizing
-- is already a promise to the agent, attached has nothing to take back. Status
-- stays with the engine: observed >= desired now holds, so MarkVolumeAttached
-- settles it on the next tick. No row = not pending anymore (or nothing to
-- revert to); the caller reports that as a retry, not a not-found.
-- name: RevertVolumeSize :one
UPDATE volumes
SET desired_size_bytes          = previous_desired_size_bytes,
    previous_desired_size_bytes = NULL
WHERE service_id = @service_id AND mount_path = @mount_path
  AND status = 'resize_pending'
  AND previous_desired_size_bytes IS NOT NULL
RETURNING *;

-- name: GetVolume :one
SELECT * FROM volumes
WHERE service_id = @service_id AND mount_path = @mount_path;

-- The host a placed volume sits on: the CLI's resize advisory sums the
-- committed bytes of the host's volumes (ListVolumesByHost + Committed()) in
-- Go against the same DiskBudget the placer uses, so the two never disagree
-- on "fits".
-- name: GetHost :one
SELECT * FROM hosts WHERE id = @host_id;

-- name: DeleteVolume :one
DELETE FROM volumes
WHERE service_id = @service_id AND mount_path = @mount_path
RETURNING *;
