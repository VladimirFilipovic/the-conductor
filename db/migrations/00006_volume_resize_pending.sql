-- +goose Up

-- resize_pending is the engine's "grow requested, host has no room yet"
-- state. Until now that case was an attached volume with desired > observed,
-- which the operator could only spot by comparing two size columns and which
-- the CLI could only take back by writing desired down to the on-disk size.
-- Naming the state lets `volume list` show it and gives `volume revert` an
-- unambiguous target: the one state where the request is not yet a promise
-- to the agent (resizing) and not yet converged (attached).
ALTER TABLE volumes DROP CONSTRAINT volumes_status_check;
ALTER TABLE volumes ADD CONSTRAINT volumes_status_check
	CHECK (status IN ('pending', 'attached', 'detached', 'resize_pending', 'resizing', 'failed'));

-- previous_desired_size_bytes is the one-shot revert target: `volume update`
-- fills it with the size it replaced, `volume revert` restores it and clears
-- the column. NULL = nothing to revert to. Kept as a column rather than a
-- history table because one grow is in flight at a time by design.
ALTER TABLE volumes ADD COLUMN previous_desired_size_bytes bigint;

-- +goose Down
ALTER TABLE volumes DROP COLUMN previous_desired_size_bytes;
-- Any resize_pending volume was attached before this state existed.
UPDATE volumes SET status = 'attached' WHERE status = 'resize_pending';
ALTER TABLE volumes DROP CONSTRAINT volumes_status_check;
ALTER TABLE volumes ADD CONSTRAINT volumes_status_check
	CHECK (status IN ('pending', 'attached', 'detached', 'resizing', 'failed'));
