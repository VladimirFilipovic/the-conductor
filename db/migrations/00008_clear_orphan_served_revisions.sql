-- +goose Up

-- Before orphan drains cleared the pointer, a region dropped between versions
-- kept pointing at the superseded deployment after its replicas were reaped,
-- so status read "serving vN" for a region serving nothing.
DELETE FROM served_revisions sr
WHERE NOT EXISTS (
	SELECT 1
	FROM deployments d
	JOIN deployment_regions dr ON dr.deployment_id = d.id
	WHERE d.environment_service_id = sr.environment_service_id
	  AND d.is_current
	  AND dr.region = sr.region
);

-- +goose Down
-- Data cleanup only: the deleted pointers were stale, nothing to restore.
