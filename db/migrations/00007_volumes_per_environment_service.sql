-- +goose Up

-- A volume is what an environment isolates: one service bound into two
-- environments must not share a disk, and the engine keys every stateful
-- placement by (environment_service, region) already — a volume keyed by
-- service alone made two environments' replicas fight over one lease, and
-- the second never came up. So the owner moves from services to
-- environment_services, and the uniqueness of name / mount_path with it.
--
-- No backfill: the database is dev-only and a service-owned volume has no
-- single environment_services row to belong to (which environment gets the
-- disk?). Existing volumes go, along with what points at them: leases first,
-- and the replicas pinned to them are unpinned so the reconcile loop reaps
-- and recreates them once a new volume exists.
UPDATE replicas SET volume_id = NULL WHERE volume_id IS NOT NULL;
DELETE FROM volume_leases;
DELETE FROM volumes;

-- Dropping service_id drops the two UNIQUE constraints that included it.
ALTER TABLE volumes DROP COLUMN service_id;
ALTER TABLE volumes ADD COLUMN environment_service_id uuid NOT NULL
	REFERENCES environment_services ON DELETE RESTRICT;
ALTER TABLE volumes ADD CONSTRAINT volumes_es_name_key  UNIQUE (environment_service_id, name);
ALTER TABLE volumes ADD CONSTRAINT volumes_es_mount_key UNIQUE (environment_service_id, mount_path);
-- The UNIQUE above indexes the leading column already; kept named so the FK
-- restrict check and ListVolumesByEnvironmentService read off it explicitly.
CREATE INDEX volumes_environment_service_id_idx ON volumes (environment_service_id);

-- +goose Down
-- Same one-way street back: nothing maps an environment-owned volume onto a
-- service-owned one, so the rows go again.
UPDATE replicas SET volume_id = NULL WHERE volume_id IS NOT NULL;
DELETE FROM volume_leases;
DELETE FROM volumes;
DROP INDEX volumes_environment_service_id_idx;
ALTER TABLE volumes DROP COLUMN environment_service_id;
ALTER TABLE volumes ADD COLUMN service_id uuid NOT NULL REFERENCES services ON DELETE RESTRICT;
ALTER TABLE volumes ADD CONSTRAINT volumes_service_id_name_key       UNIQUE (service_id, name);
ALTER TABLE volumes ADD CONSTRAINT volumes_service_id_mount_path_key UNIQUE (service_id, mount_path);
