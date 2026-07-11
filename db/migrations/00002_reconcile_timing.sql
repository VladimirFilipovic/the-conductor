-- +goose Up

-- drained_at stamps when the orchestrator emitted IntentDrain. The drain-window
-- rule reaps a replica once drained_at + drain_seconds < now(); NULL until drained.
ALTER TABLE replicas ADD COLUMN drained_at timestamptz;

-- progress_deadline caps how long the health gate may stay open (a new replica up
-- but never healthy, and never crashing to trip restart_max). Past it the rollout
-- trips to failed (stalled). Seconds; per-deployment like drain_seconds/restart_max.
ALTER TABLE deployments ADD COLUMN progress_deadline int NOT NULL DEFAULT 600
	CHECK (progress_deadline >= 0);

-- health_checks_passed_at marks when a replica FIRST passed its health probe.
-- NULL = never passed → the stalled-rollout signal the progress_deadline rule keys
-- on. Distinct from healthy (which can flip back false on a later crash): this is a
-- once-only high-water mark, so "up but never healthy" stays detectable.
ALTER TABLE replicas ADD COLUMN health_checks_passed_at timestamptz;

-- Stamp it once, on the healthy false→true edge, so no write path has to remember
-- to set it (same philosophy as set_updated_at). Guard on IS NULL keeps it a
-- high-water mark: a heal→crash→heal cycle never overwrites the first pass.
-- +goose StatementBegin
CREATE FUNCTION set_health_checks_passed_at() RETURNS trigger AS $$
BEGIN
	IF NEW.healthy AND NOT OLD.healthy AND NEW.health_checks_passed_at IS NULL THEN
		NEW.health_checks_passed_at := now();
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER replicas_set_health_checks_passed_at
	BEFORE UPDATE ON replicas
	FOR EACH ROW EXECUTE FUNCTION set_health_checks_passed_at();

-- +goose Down
DROP TRIGGER replicas_set_health_checks_passed_at ON replicas;
DROP FUNCTION set_health_checks_passed_at;
ALTER TABLE replicas DROP COLUMN health_checks_passed_at;
ALTER TABLE deployments DROP COLUMN progress_deadline;
ALTER TABLE replicas DROP COLUMN drained_at;
